package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceIdentityAndLifetime(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	for i, kind := range []string{"mount", "forward"} {
		task, _ := registerTestResource(t, tasks, kind, "work", fmt.Sprintf("%032x", i+1))
		r, err := findRunningResource(kind, "work")
		if err != nil || r.UID != task.record.UID {
			t.Fatalf("resource: %+v %v", r, err)
		}
		if err := stopResource(kind, "work", "replacement"); err == nil {
			t.Fatal("accepted replacement")
		}
		if err := stopResource(kind, "work", resourceIdentity(r)); err != nil {
			t.Fatal(err)
		}
		if rows, err := collectRunningResources(""); err != nil || len(rows) != 0 {
			t.Fatalf("ended resources retained: %+v %v", rows, err)
		}
	}
}

func TestKillPendingRequest(t *testing.T) {
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "nonexistent-ssh"))
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	startRequestDaemon(t)
	id, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"kill", "review/" + id, "extra"}, {"review", id, "--reject"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}

	if _, err := readRequest(id); err != nil {
		t.Fatal("invalid invocation consumed request", err)
	}
	if code, err := run([]string{"kill", "review/" + id}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := readRequest(id); err == nil {
		t.Fatal(err)
	}
	if _, err := run([]string{"kill", "review/" + id}); err == nil {
		t.Fatal("cancelled twice")
	}
	for _, args := range [][]string{{"ftp", "host"}, {"approve"}, {"stop", "forward/x"}, {"cancel", "host", "work"}, {"kill"}, {"kill", "host"}, {"kill", "peer/forward/x"}, {"review", "--list"}, {"mount", "--list"}, {"forward", "--list"}} {
		if _, err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestInventoryDoesNotExposeCapabilities(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	task, _ := registerTestResource(t, tasks, "forward", "web", strings.Repeat("a", 32))
	record, err := existingDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	rows := inspectResources([]resourceRecord{task.record})
	b, e := json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), record.Token) || strings.Contains(string(b), record.Address) || rows[0].Worker != "running" || rows[0].Target != "unchecked" {
		t.Fatal(string(b))
	}
}

func TestInventoryBoundedStatusWait(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	release := make(chan struct{})
	defer close(release)
	var records []resourceRecord
	for i := range 24 {
		task, _ := registerTestResource(t, tasks, "forward", fmt.Sprintf("web-%d", i), fmt.Sprintf("%032x", i+1))
		task.control = func(string) resourceReply { <-release; return resourceReply{} }
		records = append(records, task.record)
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

func TestResourceReceiptUsesInstanceRatherThanAlias(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := resourceStateDir("forward")
	if err != nil {
		t.Fatal(err)
	}
	old := resourceRecord{UID: strings.Repeat("a", 32), Kind: "forward", Name: "web", Host: "work"}
	receipt := resourceResult{Record: old, DisplayName: "web", Finished: time.Now()}
	if err := writeStateJSON(filepath.Join(dir, old.storageName()+".json.result"), receipt); err != nil {
		t.Fatal(err)
	}
	// Reusing an alias must preserve the previous instance's failure receipt.
	replacement := old
	replacement.UID = strings.Repeat("b", 32)
	if rows := collectResourceResults("", []resourceView{viewRecord(replacement)}); len(rows) != 1 {
		t.Fatalf("reused alias hid previous instance: %+v", rows)
	}
	// Renaming an instance must not make its receipt appear beside its live row.
	renamed := old
	renamed.Name = "renamed"
	if rows := collectResourceResults("", []resourceView{viewRecord(renamed)}); len(rows) != 0 {
		t.Fatalf("renamed instance duplicated: %+v", rows)
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
	r, e := resourceHTTPContext(context.Background(), l.Addr().String(), "wrong", "POST", "/stop", nil)
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
	r, e = resourceHTTPContext(context.Background(), l.Addr().String(), "secret", "POST", "/stop", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.Status)
	}
	if _, e = resourceHTTPContext(context.Background(), "example.com:80", "secret", "POST", "/stop", nil); e == nil {
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
	connection := mountSSHOptions(o)
	args := strings.Join(connection.Args, " ") + " " + connection.Command
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

func TestResourceHTTPReuseAndCancellation(t *testing.T) {
	peers := make(chan string, 2)
	waiting := make(chan struct{})
	aborted := make(chan struct{})
	l, server, err := resourceListen("secret", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ask" {
			close(waiting)
			<-r.Context().Done()
			close(aborted)
			return
		}
		peers <- r.RemoteAddr
		io.WriteString(w, "ok")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer resourceHTTPTransport.CloseIdleConnections()
	for range 2 {
		response, err := resourceHTTPContext(context.Background(), l.Addr().String(), "secret", "POST", "/setup-log", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if first, second := <-peers, <-peers; first != second {
		t.Fatalf("connection not reused: %s, %s", first, second)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		response, err := resourceHTTPContext(ctx, l.Addr().String(), "secret", "POST", "/ask", nil)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("approval request did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled approval succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval cancellation blocked")
	}
	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("server retained cancelled request")
	}
}
