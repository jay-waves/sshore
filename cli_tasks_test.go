package main

import (
	"bytes"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShortTaskExplicitHostScopesSharedIdentity(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	uid := strings.Repeat("a", 32)
	tasks := startRequestDaemon(t)
	registerTestShell(t, tasks, "local-copy", uid)
	if err := saveShellSnapshot(shellSnapshot{Host: "remote", Name: "remote-copy", Instance: uid, State: "detached"}); err != nil {
		t.Fatal(err)
	}
	if _, err := assignTaskIDs([]string{"task:" + uid}); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseCLI([]string{"remote", uid[:6]}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || target.host != "remote" || target.name != "remote-copy" {
		t.Fatalf("explicit host selected another scope: %+v %v", target, err)
	}
}

func TestShortTaskShellSelectors(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	local := registerTestShell(t, tasks, "work", strings.Repeat("a", 32))
	remote := shellSnapshot{Host: "host", Name: "build", Instance: "original", State: "detached"}
	if err := saveShellSnapshot(remote); err != nil {
		t.Fatal(err)
	}
	remoteKey := remoteShellIdentity(remote.Host, remote.Instance)
	ids, err := assignTaskIDs([]string{local.identity(), remoteKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command    string
		args, want []string
		identity   string
	}{
		{"attach", []string{ids[local.identity()].code}, []string{"work"}, local.identity()},
		{"cancel", []string{"--local", ids[local.identity()].code}, []string{"work"}, local.identity()},
		{"rename", []string{ids[local.identity()].code, "new"}, []string{"work", "new"}, local.identity()},
		{"attach", []string{ids[remoteKey].code}, []string{"host", "build"}, remoteKey},
		{"cancel", []string{"host", ids[remoteKey].code}, []string{"host", "build"}, remoteKey},
		{"rename", []string{ids[remoteKey].code, "new"}, []string{"host", "build", "new"}, remoteKey},
		{"rename", []string{"host", ids[remoteKey].code, "new"}, []string{"host", "build", "new"}, remoteKey},
	} {
		parsed, err := parseCLI(tc.args, tc.command)
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask(tc.command, &parsed)
		if err != nil || !ok || target.identity != tc.identity || !reflect.DeepEqual(parsed.operands, tc.want) {
			t.Fatalf("%s %v: %+v %v %v", tc.command, tc.args, target, parsed, err)
		}
		o, err := shellOptions(tc.command, parsed)
		if err != nil || o.session == "" {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, args := range [][]string{{"other", ids[remoteKey].code}, {"--local", ids[remoteKey].code}} {
		parsed, err := parseCLI(args, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = resolveShortTask("cancel", &parsed); err == nil {
			t.Fatal("accepted wrong scope", args)
		}
	}
	// A reused name must never make the old code act on its replacement.
	close(tasks.records[local.UID].(*nativeShellTask).shell.done)
	registerTestShell(t, tasks, "work", strings.Repeat("b", 32))
	if _, err = run([]string{"cancel", ids[local.identity()].code}); err == nil {
		t.Fatal("accepted replaced local shell")
	}
	if err = renameShellSnapshot("host", "build", "renamed"); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseCLI([]string{ids[remoteKey].code}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := resolveShortTask("attach", &parsed)
	if err != nil || !ok || target.name != "renamed" {
		t.Fatalf("%+v %v", target, err)
	}
	remote.Name = "renamed"
	remote.Instance = "replacement"
	if err = saveShellSnapshot(remote); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", ids[remoteKey].code}); err == nil {
		t.Fatal("accepted replaced remote shell")
	}
}

func TestShortTaskReviewLifecycle(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "no-ssh"))
	startRequestDaemon(t)
	id, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	key := resourceIdentity(resourceRecord{UID: id, Kind: "review", Token: id})
	ids, err := assignTaskIDs([]string{key})
	if err != nil {
		t.Fatal(err)
	}
	code := ids[key].code
	parsed, err := parseCLI([]string{code}, "review")
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := resolveShortTask("review", &parsed)
	if err != nil || !ok || target.requestID != id {
		t.Fatalf("%+v %v", target, err)
	}
	if _, err = run([]string{"attach", code}); err == nil {
		t.Fatal("attached review")
	}
	if _, err = run([]string{"rename", code, "renamed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", code}); err != nil {
		t.Fatal(err)
	}

	if _, err = readRequest(id); err == nil {
		t.Fatal("request not consumed", err)
	}
	if _, err = run([]string{"cancel", code}); err == nil {
		t.Fatal("reused ended request")
	}
}

func TestShortTaskResourceRenameAndReplacement(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	for i, kind := range []string{"mount", "forward"} {
		task, done := registerTestResource(t, tasks, kind, kind+"job", strings.Repeat(string(rune('a'+i)), 32))
		r := task.record
		key := resourceIdentity(r)
		ids, err := assignTaskIDs([]string{key})
		if err != nil {
			t.Fatal(err)
		}
		code := ids[key].code
		if _, err = run([]string{"rename", code, kind + "new"}); err != nil {
			t.Fatal(err)
		}
		parsed, err := parseCLI([]string{kind + "/" + code}, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask("cancel", &parsed)
		if err != nil || !ok || target.name != kind+"new" {
			t.Fatalf("%+v %v", target, err)
		}
		close(done)
		registerTestResource(t, tasks, kind, r.Name, strings.Repeat(string(rune('c'+i)), 32))
		if err = cancelResourceIdentity(kind+"/"+r.Name, key); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatal("accepted replacement", err)
		}
		if _, err = run([]string{"rename", code, "other"}); err == nil {
			t.Fatal("renamed replacement by old ID")
		}
	}
}

func TestShortTaskUnknownAndScopedNames(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, args := range [][]string{{"attach", "abcdef"}, {"cancel", "123456"}, {"rename", "ABCDEF", "new"}, {"review", "abcdef"}} {
		if code, err := run(args); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown task ID") {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"--local", "abcdef"}, {"host", "abcdef"}, {"review/abcdef"}} {
		parsed, err := parseCLI(args, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		if _, selected, err := resolveShortTask("cancel", &parsed); selected || err != nil {
			t.Fatalf("hex name rejected: %v %v", args, err)
		}
	}
	startRequestDaemon(t)
	id, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"rename", "review/" + id, "abcdef"}); err == nil {
		t.Fatal("hex alias accepted")
	}

}

func TestShortTaskLocalCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	r := registerTestShell(t, tasks, "work", "123456"+strings.Repeat("a", 26))
	if _, err := assignTaskIDs([]string{r.identity()}); err != nil {
		t.Fatal(err)
	}
	requests := make(chan message, 2)
	tasks.records[r.UID].(*nativeShellTask).serve = func(c net.Conn, m message) error {
		requests <- m
		reply := "cancelled"
		if m.Type == "rename" {
			reply = "renamed"
		}
		return writeMessage(c, message{Type: reply})
	}
	for _, command := range []string{"cancel", "rename"} {
		args := []string{command, "123456"}
		if command == "rename" {
			args = append(args, "new")
		}
		code, err := run(args)
		if code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
		m := <-requests
		if m.Type != command || command == "rename" && m.Name != "new" {
			t.Fatalf("wrong route: %+v", m)
		}
	}
}

func TestShortTaskRemoteHistoryPrefersCurrentName(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	now := time.Now()
	for _, s := range []shellSnapshot{
		{Host: "work", Name: "old", Instance: "instance", State: "detached", ConfirmedAt: now},
		{Host: "work", Name: "older", Instance: "instance", State: "detached", ConfirmedAt: now.Add(-time.Hour)},
	} {
		if err := saveShellSnapshot(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := cacheRemoteShells("work", inventory{Shells: []shellView{{Name: "new", Instance: "instance", State: "detached"}}}); err != nil {
		t.Fatal(err)
	}
	target, err := findTaskIdentity(remoteShellIdentity("work", "instance"))
	if err != nil || target.name != "new" {
		t.Fatalf("%+v %v", target, err)
	}
}

func TestShortTaskRejectsInvalidIndex(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := stateDir("tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []taskIDIndex{
		{Next: 1, IDs: map[string]uint64{}, Codes: map[string]string{"peer:one": "abcdef"}},
		{Next: 3, IDs: map[string]uint64{"peer:one": 1, "peer:two": 2}, Codes: map[string]string{"peer:one": "abcdef", "peer:two": "abcdef"}},
	} {
		if err = writeStateJSON(filepath.Join(dir, "ids.json"), index); err != nil {
			t.Fatal(err)
		}
		if _, err = run([]string{"cancel", "abcdef"}); err == nil || !strings.Contains(err.Error(), "invalid task short ID index") {
			t.Fatal("invalid index accepted", err)
		}
	}
}

func TestShortTaskRejectsInvalidConnection(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	snapshot := shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", Connection: &recentConnection{Host: "other", Session: "job", Server: "sshore"}}
	if err := saveShellSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	ids, err := assignTaskIDs([]string{remoteShellIdentity("work", "instance")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", ids[remoteShellIdentity("work", "instance")].code}); err == nil || !strings.Contains(err.Error(), "invalid shell connection host") {
		t.Fatal("mismatched route accepted", err)
	}
}

func TestKillOptions(t *testing.T) {
	o, err := parseOptions([]string{"cancel", "host", "build", "--server", "/a b/sshore", "--", "-p", "2222"})
	if err != nil || o.command != "cancel" || o.session != "build" || o.list {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{
		{"cancel"}, {"cancel", "host"}, {"cancel", "host", "../other"},
		{"cancel", "host", "one", "two"}, {"cancel", "host", "one", "--quiet"},
		{"cancel", "host", "one", "--download-dir", "/tmp"}, {"cancel", "host", "one", "--no-escape"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"cancel"}, &out); err != nil || !strings.Contains(out.String(), "without confirmation") {
		t.Fatal(out.String(), err)
	}
}

func TestRenameOptions(t *testing.T) {
	for _, args := range [][]string{{"rename", "host", "old", "new"}, {"rename", "host", "old", "new", "--", "-p22"}} {
		o, err := parseOptions(args)
		if err != nil || o.session != "old" || o.newName != "new" {
			t.Fatal(o, err)
		}
	}
	for _, args := range [][]string{{"rename", "host", "old"}, {"rename", "host", "old", "new", "extra"}, {"rename", "host", "old", "bad/name"}, {"rename", "host", "old", "new", "--quiet"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}

	o, err := parseOptions([]string{"rename", "12", "old", "new"})
	if err != nil || o.host != "12" || o.session != "old" || o.newName != "new" {
		t.Fatalf("numeric host: %+v %v", o, err)
	}

}

func TestCancelAllScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		host     string
		all, bad bool
	}{
		{[]string{"--all"}, "", true, false},
		{[]string{"fedora", "--all"}, "fedora", true, false},
		{[]string{"--all", "fedora", "--", "-p", "2222"}, "fedora", true, false},
		{[]string{"--local", "--all"}, "", true, false},
		{[]string{"fedora", "shell", "--all"}, "", true, true},
		{[]string{"--all", "--all"}, "", true, true},
		{[]string{"--all", "--json"}, "", true, true},
		{[]string{"fedora", "shell", "--", "--all"}, "", false, false},
	} {
		o, all, err := parseCancelAll(tc.args)
		if all != tc.all || (err != nil) != tc.bad || (!tc.bad && all && o.host != tc.host) {
			t.Errorf("%v: host=%q all=%t err=%v", tc.args, o.host, all, err)
		}
	}
}

func TestListNumbersAreNotSelectors(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, command := range []string{"attach", "cancel", "review"} {
		code, err := run([]string{command, "1"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "display-only") {
			t.Fatalf("%s: %d %v", command, code, err)
		}
	}
}
