package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	daemonUpgrade        = "sshore-stream"
	daemonBodyLimit      = 1 << 20
	daemonRequestTimeout = 40 * time.Second
	daemonSnapshotRoute  = "/admin/snapshot"
	daemonShutdownRoute  = "/admin/shutdown"
)

func daemonRequest(ctx context.Context, record localShellRecord, path string, input any) (*http.Request, error) {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+record.Address+path, &body)
	if err != nil {
		return nil, err
	}
	if body.Len() != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+record.Token)
	req.Header.Set("Sshore-Version", version)
	return req, nil
}

var daemonHTTPClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// daemonJSON sends a daemon request and decodes its response when output is non-nil.
func daemonJSON(ctx context.Context, r localShellRecord, path string, input, output any) error {
	return daemonJSONLimit(ctx, r, path, input, output, 0)
}

func daemonJSONLimit(ctx context.Context, r localShellRecord, path string, input, output any, limit int64) error {
	ctx, cancel := context.WithTimeout(ctx, daemonRequestTimeout)
	defer cancel()
	req, err := daemonRequest(ctx, r, path, input)
	if err != nil {
		return err
	}
	resp, err := daemonDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		return nil
	}
	var body io.Reader = resp.Body
	if limit > 0 {
		body = io.LimitReader(body, limit)
	}
	return json.NewDecoder(body).Decode(output)
}

func daemonAuthorized(r *http.Request, record localShellRecord) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+record.Token)) == 1
}

type daemonStream struct {
	net.Conn
	reader *bufio.Reader
}

func (s daemonStream) Read(b []byte) (int, error) { return s.reader.Read(b) }

func upgradeDaemonConn(ctx context.Context, c net.Conn, record localShellRecord, path string, input any) (stream net.Conn, result error) {
	closeConn := sync.OnceFunc(func() { _ = c.Close() })
	stop := context.AfterFunc(ctx, closeConn)
	defer func() {
		// Retire handshake cancellation before the caller takes the stream.
		stop()
		if err := ctx.Err(); err != nil {
			stream, result = nil, err
		}
		if stream == nil {
			closeConn()
		}
	}()
	if err := checkPeerVersion(record.Version, "daemon"); err != nil {
		return nil, err
	}
	req, err := daemonRequest(ctx, record, path, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", daemonUpgrade)
	if err = req.Write(c); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, daemonResponseError(resp)
	}
	if resp.Header.Get("Upgrade") != daemonUpgrade {
		return nil, fmt.Errorf("invalid daemon stream upgrade response")
	}
	if resp.Header.Get("Sshore-Version") != version {
		return nil, fmt.Errorf("daemon version mismatch during stream upgrade")
	}
	return daemonStream{c, reader}, nil
}

func serveDaemonStream(w http.ResponseWriter, r *http.Request, serve func(net.Conn)) {
	if r.Header.Get("Upgrade") != daemonUpgrade || !strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
		http.Error(w, "stream upgrade required", http.StatusUpgradeRequired)
		return
	}
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\nSshore-Version: %s\r\n\r\n", daemonUpgrade, version); err != nil {
		return
	}
	if rw.Flush() != nil {
		return
	}
	serve(daemonStream{c, rw.Reader})
}

func readDaemonBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := decodeDaemonBody(w, r, daemonBodyLimit, out); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func decodeDaemonBody(w http.ResponseWriter, r *http.Request, limit int64, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	// Consume the complete body before an upgrade takes ownership of the connection.
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("invalid trailing request data")
	}
	return nil
}

func daemonDo(req *http.Request) (*http.Response, error) {
	resp, err := daemonHTTPClient.Do(req)
	if err == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		defer resp.Body.Close()
		return nil, daemonResponseError(resp)
	}
	return resp, err
}

func daemonResponseError(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if detail := strings.TrimSpace(string(body)); detail != "" {
			return fmt.Errorf("daemon: %s", detail)
		}
	}
	return fmt.Errorf("daemon: %s", resp.Status)
}

func daemonRoutes(record localShellRecord, tasks *daemonTasks, services daemonServices, stop func()) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+daemonSnapshotRoute, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tasks.adminSnapshot(record))
	})
	mux.HandleFunc("POST "+daemonShutdownRoute, func(w http.ResponseWriter, r *http.Request) {
		var expected daemonSnapshot
		err := decodeDaemonBody(w, r, daemonBodyLimit, &expected)
		if err == nil {
			err = tasks.shutdown(record, expected)
		}
		if err != nil {
			_ = json.NewEncoder(w).Encode(daemonAdminReply{Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(daemonAdminReply{Snapshot: expected})
		stop()
	})
	mux.HandleFunc("POST /tasks/native", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(tasks.nativeViews()) })
	mux.HandleFunc("POST /resources", tasks.serveResources)
	mux.HandleFunc("POST /tasks/name", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		uid, name := query.Get("uid"), query.Get("name")
		if !validTaskUID(uid) || name != "" && validSession(name) != nil {
			http.Error(w, "invalid task name selection", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tasks.availableName(uid, name, query.Get("reuse") == "1"))
	})
	mux.HandleFunc("POST /resources/stop-confirmed", tasks.serveConfirmedResourceStops)
	mux.HandleFunc("POST /resources/{action}", tasks.serveResourceMutation)
	mux.HandleFunc("POST /ready", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(true) })
	mux.HandleFunc("POST /tasks/native/open", tasks.openNative)
	mux.HandleFunc("POST /tasks/{id}/open", tasks.open)
	activityHandler := func(w http.ResponseWriter, r *http.Request) {
		var req activityRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		activity := accountActivity.Load()
		if activity == nil {
			http.Error(w, "activity unavailable", 503)
			return
		}
		if r.URL.Path == "/activity" {
			_ = json.NewEncoder(w).Encode(activity.request(req))
			return
		}
		serveDaemonStream(w, r, func(c net.Conn) { activity.serve(c, req) })
	}
	mux.HandleFunc("POST /activity", activityHandler)
	mux.HandleFunc("POST /leases", activityHandler)
	mux.HandleFunc("POST /requests/{action}", tasks.serveRequest)
	if services != nil {
		services.Routes(mux, tasks)
	}
	mux.HandleFunc("POST /launch", func(w http.ResponseWriter, r *http.Request) {
		var req localDaemonRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		if (req.Kind != "remote-shell" && req.Kind != "shell" && req.Kind != "mount" && req.Kind != "forward") || validSession(req.Record.Name) != nil || !filepath.IsAbs(req.Record.Directory) {
			http.Error(w, "invalid launch", 400)
			return
		}
		serveDaemonStream(w, r, func(c net.Conn) {
			runDaemonLaunch(c, req, record, tasks)
		})
	})
	return mux
}
