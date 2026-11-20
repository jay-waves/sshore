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

func TestConfirmKillAll(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"\n", true}, {"\r\n", true}, {"y\n", true}, {"YES\n", true}, {"kill\n", true},
		{"n\n", false}, {"no\n", false}, {"cancel\n", false}, {"\x03\n", false},
		{"", false}, {"yes", false},
	} {
		if got := confirmKillAll(strings.NewReader(tc.input)); got != tc.want {
			t.Errorf("confirmKillAll(%q) = %t, want %t", tc.input, got, tc.want)
		}
	}
}

func TestResourceIdentityAndLifetime(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	for i, kind := range []string{"mount", "forward"} {
		task, _ := registerTestResource(t, tasks, kind, "work", fmt.Sprintf("%032x", i+1))
		rows, err := collectLocalResources("", false)
		if err != nil || len(rows) != 1 || rows[0].Identity != resourceIdentity(task.record) {
			t.Fatalf("resources: %+v %v", rows, err)
		}
		r := rows[0]
		if err := stopResource(kind, "work", "replacement"); err == nil {
			t.Fatal("accepted replacement")
		}
		if err := stopResource(kind, "work", r.Identity); err != nil {
			t.Fatal(err)
		}
		if rows, err := collectLocalResources("", false); err != nil || len(rows) != 0 {
			t.Fatalf("ended resources retained: %+v %v", rows, err)
		}
	}
}

func TestInventoryDoesNotExposeCapabilities(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	registerTestResource(t, tasks, "forward", "web", strings.Repeat("a", 32))
	record, err := existingDaemonRecord()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collectLocalResources("", true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("resources: %+v %v", rows, err)
	}
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
	for i := range 24 {
		task, _ := registerTestResource(t, tasks, "forward", fmt.Sprintf("web-%d", i), fmt.Sprintf("%032x", i+1))
		task.control = func(string) resourceReply { <-release; return resourceReply{} }
	}
	start := time.Now()
	rows, err := collectLocalResources("", true)
	if err != nil || len(rows) != 24 {
		t.Fatalf("resources: %+v %v", rows, err)
	}
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
	s := &resourceService{record: resourceRecord{Kind: "mount", Name: "work", Host: "work", Token: "secret", Address: "127.0.0.1:1"}, resultPath: filepath.Join(dir, "work"+resourceResultSuffix)}
	s.saveResult(1, io.ErrUnexpectedEOF, s.record.Name)
	rows := collectResourceResults(nil)
	if len(rows) != 1 || rows[0].Transport != "failed" || rows[0].LastError != "unexpected EOF" {
		t.Fatal(rows)
	}
	b, _ := os.ReadFile(s.resultPath)
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
	receipt := struct {
		resourceResult
		DisplayName string
	}{resourceResult: resourceResult{Record: old, Finished: time.Now()}, DisplayName: "web"}
	if err := writeStateJSON(filepath.Join(dir, old.storageName()+".json.result"), receipt); err != nil {
		t.Fatal(err)
	}
	// Reusing an alias must preserve the previous instance's failure receipt.
	replacement := old
	replacement.UID = strings.Repeat("b", 32)
	if rows := collectResourceResults([]resourceView{viewRecord(replacement)}); len(rows) != 1 {
		t.Fatalf("reused alias hid previous instance: %+v", rows)
	}
	// Renaming an instance must not make its receipt appear beside its live row.
	renamed := old
	renamed.Name = "renamed"
	if rows := collectResourceResults([]resourceView{viewRecord(renamed)}); len(rows) != 0 {
		t.Fatalf("renamed instance duplicated: %+v", rows)
	}
}

func TestFTPSessionDefaults(t *testing.T) {
	o, e := parseFTPOptions([]string{"alice@work.example:", "here:"})
	if e != nil {
		t.Fatal(e)
	}
	if o.Remote != "." || o.Mountpoint != "" || o.foreground || o.Name != "" {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	o, e = parseFTPOptions([]string{"work:project", "here:", "--foreground"})
	if e != nil || o.Name != "" || !o.foreground || o.Remote != "project" {
		t.Fatalf("options: %+v %v", o, e)
	}

}
func TestSSHAuthBrokerCapability(t *testing.T) {
	called := make(chan struct{}, 1)
	l, s, e := listenSSHAuth(context.Background(), "secret", func(w http.ResponseWriter, r *http.Request) { called <- struct{}{}; w.WriteHeader(204) })
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := postSSHAuth(context.Background(), l.Addr().String(), "wrong", "/unlock", nil)
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
	r, e = postSSHAuth(context.Background(), l.Addr().String(), "secret", "/unlock", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.Status)
	}
	if _, e = postSSHAuth(context.Background(), "example.com:80", "secret", "/unlock", nil); e == nil {
		t.Fatal("accepted non-loopback address")
	}
}

func TestFTPOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"host:/remote folder", ".:mount", "--read-only", "--", "-J", "jump"})
	if e != nil || !o.ReadOnly || o.Remote != "/remote folder" {
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
		{[]string{"work:", "here:"}, "work", ".", ""},
		{[]string{"work:", ".:"}, "work", ".", ""},
		{[]string{"work:project", "here:./local"}, "work", "project", "./local"},
		{[]string{"work:project", ".:X:"}, "work", "project", "X:"},
		{[]string{"work:project", ".:./local"}, "work", "project", "./local"},
		{[]string{"work:project", ".:./local:"}, "work", "project", "./local:"},
		{[]string{"work:/srv/name@::1", "here:"}, "work", "/srv/name@::1", ""},
		{[]string{"alice@[2001:db8::1]:/srv/project", ".:X:"}, "alice@2001:db8::1", "/srv/project", "X:"},
		{[]string{remotePathOperand("alice@2001:db8::1", "/srv/project"), ".:X:"}, "alice@2001:db8::1", "/srv/project", "X:"},
	} {
		o, err := parseFTPOptions(tc.args)
		if err != nil || o.Host != tc.host || o.Remote != tc.remote || o.Mountpoint != tc.mountpoint {
			t.Fatalf("mount %v: %+v, %v", tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"work:"}, {"work:project"}, {"work", "/src"}, {"work", "/src", "./dst"}, {"2001:db8::1", "here:"}, {"alice@2001:db8::1", "here:"}, {"work:project", "local", "extra"}, {"origin:project", "local"}, {"local:project", "origin:"}, {"[not-ip]:project", "here:"}} {
		if _, err := parseFTPOptions(args); err == nil {
			t.Fatalf("accepted mount %v", args)
		}
	}
}

func TestFTPStatsOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host:", "here:", "--stats"})
	if err != nil || !o.Stats {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestFTPNotifyOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host:", "here:", "--", "-J", "jump"})
	if err != nil {
		t.Fatal(err)
	}
	connection := mountSSHOptions(o)
	args := strings.Join(connection.Args, " ") + " " + connection.Command
	if !strings.Contains(args, "mount-agent") || !strings.Contains(args, "$HOME/.local/bin/sshore") || !strings.Contains(args, "-J jump") || strings.Contains(args, "SessionType=subsystem") {
		t.Fatalf("%+v %s", o, args)
	}
	if _, err := parseFTPOptions([]string{"host:", "here:", "--no-watch"}); err == nil {
		t.Fatal("removed compatibility flag accepted")
	}

}

func TestFTPNotifyRecovery(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	if _, err := run(remoteServiceArgs(remoteServiceRequest{Operation: "mount-agent"})); err == nil {
		t.Fatal("mount agent allowed in recovery")
	}
}

func TestRemoteMountRequest(t *testing.T) {
	for _, args := range [][]string{{".:.", "origin:"}, {"here:", "::"}, {"--read-only", ".:/tmp", "origin:/client/mount"}, {".:/tmp", "::/client/mount"}} {
		op, err := remoteMountRequest(args)
		if err != nil || op.Kind != "mount" {
			t.Fatalf("%+v %v", op, err)
		}
	}
	for _, args := range [][]string{{"origin:", "."}, {".", "origin:", "--no-watch"}, {".", "origin:", "--foreground"}, {".", "origin:", "--", "-J", "jump"}, {"host:/tmp", "origin:"}, {"origin:", "origin:"}, {".", "origin:", "extra"}} {
		if _, err := remoteMountRequest(args); err == nil {
			t.Fatal(args)
		}
	}
}

func TestAbsoluteMountPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct{ input, want string }{
		{"~", home},
		{"~/ a directory ", filepath.Join(home, " a directory ")},
		{"./~", "~"},
		{"~alice", "~alice"},
	} {
		want, err := filepath.Abs(tc.want)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := absoluteLocalPath(tc.input); err != nil || got != want {
			t.Errorf("absoluteLocalPath(%q) = %q, %v; want %q", tc.input, got, err, want)
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
	o, e := parseFTPOptions([]string{"work:", "here:"})
	if e != nil || o.IOTimeout != time.Minute || o.StartupTimeout != 0 {
		t.Fatal(o, e)
	}
	o, e = parseFTPOptions([]string{"work:", "here:", "--io-timeout", "0s", "--startup-timeout", "2m"})
	if e != nil || o.IOTimeout != 0 || o.StartupTimeout != 2*time.Minute {
		t.Fatal(o, e)
	}
	for _, a := range [][]string{{"work:", "here:", "--io-timeout", "-1s"}, {"work:", "here:", "--startup-timeout", "bad"}} {
		if _, e := parseFTPOptions(a); e == nil {
			t.Fatal(a)
		}
	}
}

func TestSSHAuthHTTPReuseAndCancellation(t *testing.T) {
	peers := make(chan string, 2)
	waiting := make(chan struct{})
	aborted := make(chan struct{})
	l, server, err := listenSSHAuth(context.Background(), "secret", func(w http.ResponseWriter, r *http.Request) {
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
	defer sshAuthHTTPClient.CloseIdleConnections()
	for range 2 {
		response, err := postSSHAuth(context.Background(), l.Addr().String(), "secret", "/unlock", nil)
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
		response, err := postSSHAuth(ctx, l.Addr().String(), "secret", "/ask", nil)
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
