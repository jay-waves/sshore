package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/pkg/sftp"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwardTransportOptions(t *testing.T) {
	f, args, fg, e := parseForwardCommand([]string{"work", "8080", "to", "3000", "--name", "web", "--", "-Jjump", "-i", "key with spaces", "-p2222", "-F", "config"})
	if e != nil || fg || f.Name != "web" || len(args) != 4 || strings.Join(f.SSH, "|") != "-J|jump|-i|key with spaces|-p|2222|-F|config" {
		t.Fatal(f, args, fg, e)
	}
	command := strings.Join(backgroundForwardArgs(f), " ")
	if !strings.Contains(command, "-J jump") || !strings.Contains(command, "-L 127.0.0.1:8080:127.0.0.1:3000") {
		t.Fatal(command)
	}
	for _, bad := range [][]string{{"work", "to", "3000", "-i"}, {"work", "to", "3000", "-D", "1080"}, {"work", "to", "3000", "--unknown"}} {
		if _, _, _, e := parseForwardCommand(bad); e == nil {
			t.Fatal(bad)
		}
	}
}
func TestInventoryDoesNotExposeCapabilities(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	token := "PRIVATE-CONTROL-TOKEN"
	l, server, e := resourceListen(token, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resourceStatus{Version: version, Transport: "connected", Target: "unchecked"})
	})
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	defer l.Close()
	rows := inspectResources([]resourceRecord{{Kind: "forward", Name: "web", Host: "work", Address: l.Addr().String(), Token: token, Version: version}})
	b, e := json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), token) || strings.Contains(string(b), l.Addr().String()) || rows[0].Worker != "running" || rows[0].Target != "unchecked" {
		t.Fatal(string(b))
	}
}
func TestInventoryBoundedStatusWait(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	l, server, e := resourceListen("x", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	defer l.Close()
	var records []resourceRecord
	for i := 0; i < 24; i++ {
		records = append(records, resourceRecord{Kind: "forward", Name: "web", Address: l.Addr().String(), Token: "x"})
	}
	start := time.Now()
	rows := inspectResources(records)
	if time.Since(start) > 3*time.Second {
		t.Fatal("status checks exceeded aggregate deadline")
	}
	for _, r := range rows {
		if r.Worker != "unavailable" {
			t.Fatal(r)
		}
	}
}
func TestSFTPRequestTimeoutIgnoresHealthySibling(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	packet := make([]byte, 5)
	packet[0] = 3
	binary.BigEndian.PutUint32(packet[1:], 1)
	if e := wire.record(packet); e != nil {
		t.Fatal(e)
	}
	fired := make(chan struct{})
	watch := &sftpWatch{}
	stop := wire.watchRequests(30*time.Millisecond, watch, func() { close(fired) })
	defer stop()
	// Keep a second request/reply moving; it must not reset the stuck request's age.
	for i := uint32(2); i < 6; i++ {
		binary.BigEndian.PutUint32(packet[1:], i)
		if e := wire.record(packet); e != nil {
			t.Fatal(e)
		}
		packet[0] = 101
		if _, e := wire.filter(packet); e != nil {
			t.Fatal(e)
		}
		packet[0] = 3
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("request failed to time out")
	}
	if watch.err() == nil || !strings.Contains(watch.err().Error(), "no retry") {
		t.Fatal(watch.err())
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
func TestSFTPIdleDoesNotTimeout(t *testing.T) {
	wire := newFTPWire(strings.NewReader(""), nopWriteCloser{io.Discard})
	watch := &sftpWatch{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := wire.watchRequests(10*time.Millisecond, watch, cancel)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("idle connection aborted")
	case <-time.After(35 * time.Millisecond):
	}
}
func TestTransferProgressEstimate(t *testing.T) {
	now := time.Now()
	p := transferProgress{at: now}
	s := p.sample(now.Add(time.Second), 1024, 2048)
	if !strings.Contains(s, "remaining 1s") {
		t.Fatal(s)
	}
	if !strings.Contains(p.sample(now.Add(2*time.Second), 2048, 2048), "remaining 0s") {
		t.Fatal("completion ETA")
	}
}
func TestResourceFailureReceipt(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, e := resourceStateDir("mount")
	if e != nil {
		t.Fatal(e)
	}
	s := &resourceService{record: resourceRecord{Kind: "mount", Name: "work", Host: "work", Token: "secret", Address: "127.0.0.1:1"}, recordPath: filepath.Join(dir, "work.json")}
	s.saveResult(1, io.ErrUnexpectedEOF)
	rows := collectResourceResults("", nil)
	if len(rows) != 1 || rows[0].Transport != "failed" || rows[0].LastError != "unexpected EOF" {
		t.Fatal(rows)
	}
	b, _ := os.ReadFile(s.recordPath + ".result")
	if strings.Contains(string(b), "secret") {
		t.Fatal("receipt leaked capability")
	}
}

func TestSFTPTimeoutUnblocksPendingCall(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		init := make([]byte, 9)
		if _, e := io.ReadFull(a, init); e != nil {
			return
		}
		a.Write([]byte{0, 0, 0, 5, 2, 0, 0, 0, 3}) // VERSION 3
		io.Copy(io.Discard, a)                     // Consume requests, never respond.
	}()
	wire := newFTPWire(b, b)
	watch := &sftpWatch{}
	stop := wire.watchRequests(30*time.Millisecond, watch, func() { b.Close() })
	defer stop()
	client, e := sftp.NewClientPipe(wire, wire)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, e := client.RealPath("."); done <- e }()
	select {
	case e := <-done:
		if e == nil || watch.err() == nil {
			t.Fatal(e, watch.err())
		}
	case <-time.After(time.Second):
		t.Fatal("pending SFTP call leaked")
	}
}
func TestMountTimeoutOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"work"})
	if e != nil || o.ioTimeout != time.Minute || o.startupTimeout != 0 {
		t.Fatal(o, e)
	}
	o, e = parseFTPOptions([]string{"work", "--io-timeout", "0s", "--startup-timeout", "2m"})
	if e != nil || o.ioTimeout != 0 || o.startupTimeout != 2*time.Minute {
		t.Fatal(o, e)
	}
	for _, a := range [][]string{{"work", "--io-timeout", "-1s"}, {"work", "--startup-timeout", "bad"}} {
		if _, e := parseFTPOptions(a); e == nil {
			t.Fatal(a)
		}
	}
}
