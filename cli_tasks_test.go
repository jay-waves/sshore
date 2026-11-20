package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	localCode, remoteCode := taskShortCode(local.identity()), taskShortCode(remoteKey)
	for _, tc := range []struct {
		command    string
		args, want []string
		identity   string
	}{
		{"attach", []string{localCode}, []string{"work"}, local.identity()},
		{"kill", []string{"here:" + localCode}, []string{"work"}, local.identity()},
		{"kill", []string{localCode}, []string{"work"}, local.identity()},
		{"rename", []string{localCode, "new"}, []string{"work", "new"}, local.identity()},
		{"attach", []string{remoteCode}, []string{"host", "build"}, remoteKey},
		{"kill", []string{"host:" + remoteCode}, []string{"host", "build"}, remoteKey},
		{"rename", []string{remoteCode, "new"}, []string{"host", "build", "new"}, remoteKey},
		{"rename", []string{"host", remoteCode, "new"}, []string{"host", "build", "new"}, remoteKey},
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
	for _, args := range [][]string{{"other", remoteCode}, {"here:" + remoteCode}, {remoteCode}} {
		parsed, err := parseCLI(args, "kill")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = resolveShortTask("kill", &parsed); err == nil {
			t.Fatal("accepted wrong scope", args)
		}
	}
	parsed, err := parseCLI([]string{remote.Name}, "kill")
	if err != nil {
		t.Fatal(err)
	}
	if _, selected, err := resolveShortTask("kill", &parsed); selected || err != nil {
		t.Fatalf("remote alias selected by bare kill: selected=%t err=%v", selected, err)
	}
	// A reused name must never make the old code act on its replacement.
	close(tasks.records[local.UID].(*nativeShellTask).shell.done)
	registerTestShell(t, tasks, "work", strings.Repeat("b", 32))
	if _, err = run([]string{"kill", localCode}); err == nil {
		t.Fatal("accepted replaced local shell")
	}
	if err = rememberShellRename("host", "build", "renamed", remote.Instance); err != nil {
		t.Fatal(err)
	}
	parsed, err = parseCLI([]string{remoteCode}, "attach")
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
	if _, err = run([]string{"kill", "host:" + remoteCode}); err == nil {
		t.Fatal("accepted replaced remote shell")
	}
}

func TestShortTaskReviewLifecycle(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "no-ssh"))
	startRequestDaemon(t)
	queued, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	id := queued.ID
	key := resourceIdentity(resourceRecord{UID: id, Kind: "review", Token: id})
	code := taskShortCode(key)
	parsed, err := parseCLI([]string{code}, "review")
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := resolveShortTask("review", &parsed)
	if err != nil || !ok || canonicalTaskUID(target.identity) != id {
		t.Fatalf("%+v %v", target, err)
	}
	if _, err = run([]string{"attach", code}); err == nil {
		t.Fatal("attached review")
	}
	if _, err = run([]string{"rename", code, "renamed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"kill", code}); err != nil {
		t.Fatal(err)
	}

	if _, err = findRequest(id); err == nil {
		t.Fatal("request not consumed", err)
	}
	if _, err = run([]string{"kill", code}); err == nil {
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
		code := taskShortCode(key)
		previousAlias := kind + "alias"
		if _, err := run([]string{"rename", kind + "/" + r.Name, previousAlias}); err != nil {
			t.Fatal(err)
		}
		if _, err := run([]string{"rename", code, kind + "new"}); err != nil {
			t.Fatal(err)
		}
		inv, err := collectInventory(options{})
		if err != nil || !slices.ContainsFunc(inv.Resources, func(row resourceView) bool {
			return row.Identity == key && row.ID == kind+"/"+kind+"new" && row.ShortID == code && row.Worker == "running"
		}) {
			t.Fatalf("renamed resource inventory: %+v %v", inv.Resources, err)
		}
		if _, err := run([]string{"kill", kind + "/" + previousAlias}); err == nil {
			t.Fatal("old alias still selects the resource")
		}
		parsed, err := parseCLI([]string{kind + "/" + code}, "kill")
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask("kill", &parsed)
		if err != nil || !ok || target.name != kind+"new" {
			t.Fatalf("%+v %v", target, err)
		}
		close(done)
		registerTestResource(t, tasks, kind, r.Name, strings.Repeat(string(rune('c'+i)), 32))
		if err = killResourceIdentity(kind+"/"+r.Name, key); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatal("accepted replacement", err)
		}
		if _, err = run([]string{"rename", code, "other"}); err == nil {
			t.Fatal("renamed replacement by old ID")
		}
	}
}

func TestShortTaskUnknownAndScopedNames(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, args := range [][]string{{"attach", "abcdef"}, {"kill", "123456"}, {"rename", "ABCDEF", "new"}, {"review", "abcdef"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"here:abcdef"}, {"host", "abcdef"}, {"review/abcdef"}} {
		parsed, err := parseCLI(args, "kill")
		if err != nil {
			t.Fatal(err)
		}
		if _, selected, err := resolveShortTask("kill", &parsed); selected || err != nil {
			t.Fatalf("hex name rejected: %v %v", args, err)
		}
	}
	startRequestDaemon(t)
	queued, err := queueRequest(testRequestOptions(), []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	id := queued.ID
	if _, err = run([]string{"rename", "review/" + id, "abcdef"}); err == nil {
		t.Fatal("hex alias accepted")
	}

}

func TestShortTaskLocalCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	r := registerTestShell(t, tasks, "work", "123456"+strings.Repeat("a", 26))
	requests := make(chan message, 2)
	tasks.records[r.UID].(*nativeShellTask).serve = func(c net.Conn, m message) error {
		requests <- m
		reply := "killed"
		if m.Type == "rename" {
			reply = "renamed"
		}
		return writeMessage(c, message{Type: reply})
	}
	for _, command := range []string{"kill", "rename"} {
		args := []string{command, "123456"}
		if command == "rename" {
			args = []string{command, "here:123456", "new"}
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
	activityFixture(t)
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
	parsed, err := parseCLI([]string{taskShortCode(remoteShellIdentity("work", "instance"))}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || target.name != "new" {
		t.Fatalf("%+v %v", target, err)
	}
}

func TestTaskPrefixCollisionResolution(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	first := "abcdef" + strings.Repeat("1", 26)
	second := "abcdef" + strings.Repeat("2", 26)
	registerTestShell(t, tasks, "one", first)
	registerTestShell(t, tasks, "two", second)
	for _, tc := range []struct{ ref, uid string }{
		{"ab", ""}, {"abcdef", ""}, {"abcdef1", first}, {first, first}, {second, second}, {"ABCDEF2", second},
	} {
		parsed, err := parseCLI([]string{tc.ref}, "attach")
		if err != nil {
			t.Fatal(err)
		}
		target, selected, err := resolveShortTask("attach", &parsed)
		if tc.uid == "" {
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("accepted ambiguous prefix: %v", err)
			}
		} else if err != nil || !selected || target.identity != "task:"+tc.uid {
			t.Fatalf("%s: %+v %v", tc.ref, target, err)
		}
	}

}

func TestShortTaskRejectsInvalidConnection(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	snapshot := shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", Connection: &recentConnection{Host: "other", Session: "job", Server: "sshore"}}
	dir, err := stateDir("shells")
	if err != nil {
		t.Fatal(err)
	}
	path, err := snapshot.cachePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateJSON(path, snapshot); err != nil {
		t.Fatal(err)
	}
	code := taskShortCode(remoteShellIdentity("work", "instance"))
	if _, err := run([]string{"kill", "work:" + code}); err == nil || !strings.Contains(err.Error(), "invalid shell connection identity") {
		t.Fatal("mismatched route accepted", err)
	}
}

func TestKillOptions(t *testing.T) {
	o, err := parseOptions([]string{"kill", "host", "build", "--server", "/a b/sshore", "--", "-p", "2222"})
	if err != nil || o.command != "kill" || o.session != "build" || o.list {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{
		{"kill"}, {"kill", "host"}, {"kill", "host", "../other"},
		{"kill", "host", "one", "two"}, {"kill", "host", "one", "--quiet"},
		{"kill", "host", "one", "--download-dir", "/tmp"}, {"kill", "host", "one", "--no-escape"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"kill"}, &out); err != nil || !strings.Contains(out.String(), "without confirmation") {
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

func TestKillAllScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		host     string
		all, bad bool
	}{
		{[]string{"--all"}, "", true, false},
		{[]string{"fedora:", "--all"}, "fedora", true, false},
		{[]string{"here:", "--all"}, "", true, false},
		{[]string{"fedora", "--all"}, "fedora", true, false},
		{[]string{"--all", "fedora", "--", "-p", "2222"}, "fedora", true, false},
		{[]string{"here:", "--all"}, "", true, false},
		{[]string{"fedora", "shell", "--all"}, "", true, true},
		{[]string{"--all", "--all"}, "", true, true},
		{[]string{"--all", "--json"}, "", true, true},
		{[]string{"fedora", "shell", "--", "--all"}, "", false, true},
	} {
		parsed, err := parseCLI(tc.args, "kill")
		all := parsed.has("--all")
		var o options
		if err == nil && all {
			o, err = killAllOptions(parsed)
		}
		if all != tc.all || (err != nil) != tc.bad || (!tc.bad && all && o.host != tc.host) {
			t.Errorf("%v: host=%q all=%t err=%v", tc.args, o.host, all, err)
		}
	}
}

func TestListNumbersAreNotSelectors(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, command := range []string{"attach", "kill", "review"} {
		code, err := run([]string{command, "1"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "display-only") {
			t.Fatalf("%s: %d %v", command, code, err)
		}
	}
}

func TestAttachUnknownTargetNeverConnects(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	marker := filepath.Join(t.TempDir(), "ssh-invoked")
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", ssh)
	for _, ref := range []string{"xxxyyy", "3a"} {
		code, err := run([]string{"attach", ref})
		if code != 2 || err == nil {
			t.Fatalf("unknown %s: %d %v", ref, code, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unknown target started SSH: %v", err)
	}
}

func TestTwoCharacterTaskPrefix(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	first := "3aaaaa" + strings.Repeat("1", 26)
	registerTestShell(t, tasks, "short-prefix", first)
	registerTestShell(t, tasks, "db", strings.Repeat("c", 32))
	parsed, err := parseCLI([]string{"3a"}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || target.identity != "task:"+first {
		t.Fatalf("unique short prefix: %+v %v", target, err)
	}
	alias, _ := parseCLI([]string{"db"}, "attach")
	if candidate, ok, err := resolveShortTask("attach", &alias); err != nil || !ok || candidate.name != "db" {
		t.Fatalf("short hex alias lost: %+v %v", candidate, err)
	}
	registerTestShell(t, tasks, "collision", "3abbbb"+strings.Repeat("2", 26))
	parsed, _ = parseCLI([]string{"3a"}, "attach")
	if _, selected, err := resolveShortTask("attach", &parsed); err == nil || selected || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous short prefix selected: %v", err)
	}
}
