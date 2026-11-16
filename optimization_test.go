package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwardTransportOptions(t *testing.T) {
	f, fg, e := parseForwardCommand([]string{"8080", "work:3000", "--name", "web", "--", "-Jjump", "-i", "key with spaces", "-p2222", "-F", "config"})
	if e != nil || fg || f.Name != "web" || strings.Join(f.SSH, "|") != "-J|jump|-i|key with spaces|-p|2222|-F|config" {
		t.Fatal(f, fg, e)
	}
	command := strings.Join(forwardSSHArgs(f, forwardRemoteCommand(f)), " ")
	if !strings.Contains(command, "-J jump") || !strings.Contains(command, "-L 127.0.0.1:8080:127.0.0.1:3000") {
		t.Fatal(command)
	}
	for _, bad := range [][]string{{"8080", "work:3000", "-i"}, {"8080", "work:3000", "-D", "1080"}, {"8080", "work:3000", "--unknown"}} {
		if _, _, e := parseForwardCommand(bad); e == nil {
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
