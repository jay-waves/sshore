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

func TestResourceIdentityAndLegacyMount(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, e := resourceStateDir("mount")
	if e != nil {
		t.Fatal(e)
	}
	// Existing private mount records continue to work without a Kind field.
	b, _ := json.Marshal(resourceRecord{Name: "work", Host: "work", Version: version})
	if e = os.WriteFile(filepath.Join(dir, "work.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	r, e := readResourceRecord("mount", "work")
	if e != nil || r.Kind != "mount" {
		t.Fatal(r, e)
	}
	for _, id := range []string{"work", "shell/work", "mount/../work", "review/abc"} {
		if _, e := runCancel([]string{id}); e == nil {
			t.Fatal("accepted", id)
		}
	}
	id := strings.Repeat("a", 32)
	o, e := parseReview([]string{"review/" + id})
	if e != nil || o.id != id {
		t.Fatal(o, e)
	}
}

func TestCancelPendingRequest(t *testing.T) {
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "nonexistent-ssh"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	id, err := writeApproval(options{host: "fixture", server: "sshore", session: "work"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cancel", "review/" + id, "extra"}, {"review", id, "--reject"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readApproval(dir, id); err != nil {
		t.Fatal("invalid invocation consumed request", err)
	}
	if code, err := run([]string{"cancel", "review/" + id}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := run([]string{"cancel", "review/" + id}); err == nil {
		t.Fatal("cancelled twice")
	}
	for _, args := range [][]string{{"ftp", "host"}, {"approve"}, {"stop", "forward/x"}, {"kill", "host", "work"}, {"cancel"}, {"cancel", "host"}, {"cancel", "peer/forward/x"}, {"review", "--list"}, {"mount", "--list"}, {"forward", "--list"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
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

func TestFTPSessionDefaults(t *testing.T) {
	o, e := parseFTPOptions([]string{"alice@work.example:"})
	if e != nil {
		t.Fatal(e)
	}
	if o.remote != "." || o.mountpoint != "" || o.foreground || o.name != "" {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	o, e = parseFTPOptions([]string{"work:project", "--foreground"})
	if e != nil || o.name != "" || !o.foreground || o.remote != "project" {
		t.Fatalf("options: %+v %v", o, e)
	}

}
func TestFTPControlCapability(t *testing.T) {
	called := make(chan struct{}, 1)
	l, s, e := resourceListen("secret", func(w http.ResponseWriter, r *http.Request) { called <- struct{}{}; w.WriteHeader(204) })
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := resourceHTTP(l.Addr().String(), "wrong", "POST", "/stop", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal(r.Status)
	}
	select {
	case <-called:
		t.Fatal("unauthorized control request dispatched")
	default:
	}
	r, e = resourceHTTP(l.Addr().String(), "secret", "POST", "/stop", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.Status)
	}
	if _, e = resourceHTTP("example.com:80", "secret", "POST", "/stop", nil); e == nil {
		t.Fatal("accepted non-loopback address")
	}
}

func TestFTPOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"host:/remote folder", "mount", "--read-only", "--", "-J", "jump"})
	if e != nil || !o.readOnly || o.remote != "/remote folder" {
		t.Fatalf("%+v %v", o, e)
	}
	for _, args := range [][]string{{}, {"host", "/", "mount", "-f"}, {"--", "-bad", "/", "mount"}} {
		if _, e := parseFTPOptions(args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMountSourceEndpoints(t *testing.T) {
	for _, tc := range []struct {
		args                     []string
		host, remote, mountpoint string
	}{
		{[]string{"work:"}, "work", ".", ""},
		{[]string{"work:project", "./local"}, "work", "project", "./local"},
		{[]string{"work:/srv/name@::1"}, "work", "/srv/name@::1", ""},
		{[]string{"alice@[2001:db8::1]:/srv/project", "X:"}, "alice@2001:db8::1", "/srv/project", "X:"},
		{[]string{remotePathOperand("alice@2001:db8::1", "/srv/project"), "X:"}, "alice@2001:db8::1", "/srv/project", "X:"},
	} {
		o, err := parseFTPOptions(tc.args)
		if err != nil || o.host != tc.host || o.remote != tc.remote || o.mountpoint != tc.mountpoint {
			t.Fatalf("mount %v: %+v, %v", tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"work"}, {"work", "/src"}, {"work", "/src", "./dst"}, {"2001:db8::1"}, {"alice@2001:db8::1"}, {"work:project", "local", "extra"}, {"origin:project", "local"}, {"[not-ip]:project"}} {
		if _, err := parseFTPOptions(args); err == nil {
			t.Fatalf("accepted mount %v", args)
		}
	}
}

func TestFTPStatsOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host:", "--stats"})
	if err != nil || !o.stats {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestFTPNotifyOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host:", "--server", "/a path/sshore", "--", "-J", "jump"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(mountSSHArgs(o), " ")
	if !strings.Contains(args, "mount-agent") || !strings.Contains(args, "-J jump") || strings.Contains(args, "SessionType=subsystem") {
		t.Fatalf("%+v %s", o, args)
	}
	if _, err := parseFTPOptions([]string{"host:", "--no-watch"}); err == nil {
		t.Fatal("removed compatibility flag accepted")
	}

}

func TestFTPNotifyRecovery(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	if _, err := run([]string{"mount-agent"}); err == nil {
		t.Fatal("mount agent allowed in recovery")
	}
}

func TestRemoteMountRequest(t *testing.T) {
	for _, args := range [][]string{{".", "origin:"}, {"--read-only", "/tmp", "origin:/client/mount"}} {
		op, err := remoteMountRequest(args)
		if err != nil || op.Kind != "mount" || op.LocalPath != "" {
			t.Fatalf("%+v %v", op, err)
		}
	}
	for _, args := range [][]string{{"origin:", "."}, {".", "origin:", "--no-watch"}, {".", "origin:", "--foreground"}, {".", "origin:", "--", "-J", "jump"}, {"host:/tmp", "origin:"}, {"-", "origin:"}, {".", "origin:", "extra"}} {
		if _, err := remoteMountRequest(args); err == nil {
			t.Fatal(args)
		}
	}
}

func TestMountApprovalSelection(t *testing.T) {
	input := &stubSetupInput{answers: []string{""}}
	if path, err := readMountApprovalPath(input, ""); err != nil || path != "" {
		t.Fatalf("%q %v", path, err)
	}
	if _, err := readMountApprovalPath(&stubSetupInput{}, ""); err == nil {
		t.Fatal("cancel accepted")
	}
}

func TestMountTimeoutOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"work:"})
	if e != nil || o.ioTimeout != time.Minute || o.startupTimeout != 0 {
		t.Fatal(o, e)
	}
	o, e = parseFTPOptions([]string{"work:", "--io-timeout", "0s", "--startup-timeout", "2m"})
	if e != nil || o.ioTimeout != 0 || o.startupTimeout != 2*time.Minute {
		t.Fatal(o, e)
	}
	for _, a := range [][]string{{"work:", "--io-timeout", "-1s"}, {"work:", "--startup-timeout", "bad"}} {
		if _, e := parseFTPOptions(a); e == nil {
			t.Fatal(a)
		}
	}
}
