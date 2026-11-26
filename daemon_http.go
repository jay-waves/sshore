package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	json "encoding/json/v2"
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
		if err := json.MarshalWrite(&body, input); err != nil {
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
	return json.UnmarshalRead(resp.Body, output)
}

func daemonAuthorized(r *http.Request, record localShellRecord) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+record.Token)) == 1
}

type daemonStream struct {
	net.Conn
	reader *bufio.Reader
}

func (s daemonStream) Read(b []byte) (int, error)           { return s.reader.Read(b) }
func (s daemonStream) ReadSlice(delim byte) ([]byte, error) { return s.reader.ReadSlice(delim) }

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
	if err := checkPeerVersion(resp.Header.Get("Sshore-Version"), "daemon"); err != nil {
		return nil, err
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
	if err := decodeDaemonBody(w, r, out); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func decodeDaemonBody(w http.ResponseWriter, r *http.Request, out any) error {
	// Consume the complete body before an upgrade takes ownership of the connection.
	return json.UnmarshalRead(http.MaxBytesReader(w, r.Body, daemonBodyLimit), out)
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
	if peer := resp.Header.Get("Sshore-Version"); resp.StatusCode == http.StatusConflict && peer != "" && peer != version {
		return checkPeerVersion(peer, "daemon")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if detail := strings.TrimSpace(string(body)); detail != "" {
			return fmt.Errorf("daemon: %s", detail)
		}
	}
	return fmt.Errorf("daemon: %s", resp.Status)
}

func daemonRoutes(record localShellRecord, activity *daemonActivity, stop func()) *http.ServeMux {
	tasks := activity.tasks
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+daemonSnapshotRoute, func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, tasks.adminSnapshot(record))
	})
	mux.HandleFunc("POST "+daemonShutdownRoute, func(w http.ResponseWriter, r *http.Request) {
		var expected daemonSnapshot
		err := decodeDaemonBody(w, r, &expected)
		if err == nil {
			err = tasks.shutdown(record, expected)
		}
		if err != nil {
			_ = json.MarshalWrite(w, daemonAdminReply{Error: err.Error()})
			return
		}
		_ = json.MarshalWrite(w, daemonAdminReply{Snapshot: expected})
		stop()
	})
	mux.HandleFunc("POST /tasks/snapshot", tasks.serveTaskInventory)
	mux.HandleFunc("POST /tasks/list", func(w http.ResponseWriter, r *http.Request) {
		var request taskListRequest
		if !readDaemonBody(w, r, &request) {
			return
		}
		_ = json.MarshalWrite(w, activity.listTasks(r.Context(), request))
	})
	mux.HandleFunc("POST /resources/stop-confirmed", tasks.serveConfirmedResourceStops)
	mux.HandleFunc("POST /resources/kill", tasks.serveResourceStop)
	mux.HandleFunc("POST /tasks/select", func(w http.ResponseWriter, r *http.Request) {
		var q taskQuery
		if !readDaemonBody(w, r, &q) {
			return
		}
		matches, err := q.run(tasks)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.MarshalWrite(w, matches)
	})
	mux.HandleFunc("POST /tasks/native/open", tasks.openNative)
	mux.HandleFunc("POST /tasks/{id}/open", tasks.open)
	mux.HandleFunc("POST /leases", func(w http.ResponseWriter, r *http.Request) {
		var req leaseRequest
		if readDaemonBody(w, r, &req) {
			serveDaemonStream(w, r, func(c net.Conn) { activity.serve(c, req, true) })
		}
	})
	mux.HandleFunc("POST /metadata", func(w http.ResponseWriter, r *http.Request) {
		var req metadataRequest
		if readDaemonBody(w, r, &req) {
			_ = json.MarshalWrite(w, activity.handleMetadata(req))
		}
	})
	registerPlatformDaemonRoutes(mux, tasks)
	mux.HandleFunc("POST /launch", func(w http.ResponseWriter, r *http.Request) {
		var req localDaemonRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		shell := req.Kind == "shell" || req.Kind == "remote-shell" && validSession(req.Record.Name) == nil
		resource := req.Kind == "resource" && req.Resource != nil
		if !(shell || resource) || !filepath.IsAbs(req.Record.Directory) {
			http.Error(w, "invalid launch", 400)
			return
		}
		serveDaemonStream(w, r, func(c net.Conn) {
			runDaemonLaunch(c, req, record, tasks)
		})
	})
	return mux
}
