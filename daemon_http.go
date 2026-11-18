package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"

	"net"
	"net/http"
	"time"
)

const daemonUpgrade = "sshore-stream"

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
	req.Header.Set("Authorization", "Bearer "+record.Token)
	req.Header.Set("Sshore-Version", version)
	return req, nil
}

var daemonHTTPClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func daemonJSON(ctx context.Context, r localShellRecord, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
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
	return json.NewDecoder(resp.Body).Decode(output)
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
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	defer func() {
		if result != nil {
			c.Close()
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
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != daemonUpgrade || resp.Header.Get("Sshore-Version") != version {
		return nil, fmt.Errorf("daemon stream: %s", resp.Status)
	}
	return daemonStream{c, reader}, nil
}

func serveDaemonStream(w http.ResponseWriter, r *http.Request, serve func(net.Conn)) {
	if r.Header.Get("Upgrade") != daemonUpgrade {
		http.Error(w, "stream upgrade required", http.StatusUpgradeRequired)
		return
	}
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	if _, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\nSshore-Version: %s\r\n\r\n", daemonUpgrade, version); err != nil {
		return
	}
	if rw.Flush() != nil {
		return
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	serve(daemonStream{c, rw.Reader})
}

func readDaemonBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(out); err != nil {
		http.Error(w, err.Error(), 400)
		return false
	}
	return true
}
func daemonDo(req *http.Request) (*http.Response, error) {
	resp, err := daemonHTTPClient.Do(req)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("daemon %s: %s", req.URL.Path, resp.Status)
	}
	return resp, err
}
