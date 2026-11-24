package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestRecentAttachForms(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	original := options{host: "work", session: "eevee", server: "/custom/sshore", sshDirectory: t.TempDir(), ssh: []string{"-p", "2222", "-J", "jump", "-L", "8080:localhost:80"}}
	if err := rememberConnection(original); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"attach", "work:"}, {"attach", "work:eevee"}} {
		o, err := parseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if err = resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.host != "work" || o.session != strings.TrimPrefix(args[1], "work:") {
			t.Fatalf("%v: %+v", args, o)
		}
		if o.server != "/custom/sshore" || o.sshDirectory != original.sshDirectory || !reflect.DeepEqual(o.ssh, []string{"-p", "2222", "-J", "jump"}) {
			t.Fatalf("transport not restored: %+v", o)
		}
	}
	o, err := parseOptions([]string{"attach", "work", "--server", "sshore", "--", "-p", "2200"})
	if err != nil {
		t.Fatal(err)
	}
	if err = resolveRecentAttach(&o); err != nil {
		t.Fatal(err)
	}
	if o.server != "sshore" || o.sshDirectory != "" || !reflect.DeepEqual(o.ssh, []string{"-p", "2200"}) {
		t.Fatalf("explicit options overwritten: %+v", o)
	}
}
func TestResolveBareRemoteAttachID(t *testing.T) {
	activity := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	uid := strings.Repeat("a", 32)
	shell := options{host: "work", session: "build", instance: uid, server: "sshore"}
	if err := rememberDetachedAttachment(shell); err != nil {
		t.Fatal(err)
	}

	resolve := func(ref string) (selectedTask, cliArguments, error) {
		t.Helper()
		parsed, err := parseCLI([]string{ref}, "attach")
		if err != nil {
			t.Fatal(err)
		}
		target, _, err := resolveShortTask("attach", &parsed)
		return target, parsed, err
	}
	target, parsed, err := resolve(strings.ToUpper(uid))
	if err != nil || target.Instance != uid || target.Connection == nil || parsed.local ||
		parsed.operand(0) != shell.host || parsed.operand(1) != uid {
		t.Fatalf("bare remote ID: %+v %+v %v", target, parsed, err)
	}
	target, _, err = resolve(uid[:6])
	if err != nil || target.Instance != uid || target.Connection == nil {
		t.Fatalf("short remote ID: %+v %v", target, err)
	}
	if _, _, err := resolve("here:" + uid[:6]); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("explicit local lookup: %v", err)
	}
	localID := uid[:6] + strings.Repeat("b", 26)
	local := &localShell{shellTask: shellTask{record: localShellRecord{Name: "local", UID: localID}, done: make(chan struct{})}}
	if err := activity.tasks.register(localID, &nativeShellTask{shell: local}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(uid[:6]); err == nil || !strings.Contains(err.Error(), "ambiguous task ID") {
		t.Fatalf("expected ambiguity across local and remote tasks, got %v", err)
	}
	target, parsed, err = resolve(localID[:7])
	if err != nil || !parsed.local || target.Identity != "task:"+localID || target.Connection != nil {
		t.Fatalf("longer local prefix: %+v %+v %v", target, parsed, err)
	}
	shell.host = "other"
	if err := rememberConnection(shell); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(uid); err == nil || !strings.Contains(err.Error(), "multiple saved connections") {
		t.Fatalf("expected ambiguous remote ID, got %v", err)
	}
}

func TestRecentAttachMissingAndPerHost(t *testing.T) {
	fixture := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, r := range []options{{host: "work", session: "eevee", server: "sshore"}, {host: "other", session: "pikachu", server: "sshore"}, {host: "work", session: "snorlax", server: "sshore"}} {
		if err := rememberConnection(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"work", "other", "unknown"} {
		o := options{command: "attach", host: host, server: "sshore"}
		if err := resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.session != "" {
			t.Fatalf("%q: %q", host, o.session)
		}
	}
	records, err := fixture.metadata.readRecentConnectionsLocal()
	if err != nil || len(records) != 2 {
		t.Fatalf("%+v %v", records, err)
	}
}

func TestRecentConnectionsFollowStateDirectory(t *testing.T) {
	fixture := activityFixture(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	t.Setenv("SSHORE_STATE_DIR", first)
	if err := rememberConnection(options{host: "one", session: "shell", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", second)
	if rows, err := fixture.metadata.readRecentConnectionsLocal(); err != nil || len(rows) != 0 {
		t.Fatalf("new state directory inherited recent connections: %+v %v", rows, err)
	}
	if err := rememberConnection(options{host: "two", session: "shell", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", first)
	if rows, err := fixture.metadata.readRecentConnectionsLocal(); err != nil || len(rows) != 1 || rows[0].Host != "one" {
		t.Fatalf("original recent connections lost: %+v %v", rows, err)
	}
}

func TestNativePortOverridesSavedConnection(t *testing.T) {
	activityFixture(t)
	for _, saved := range [][]string{
		{"-p", "2222", "-J", "jump", "-i", "key"},
		{"-o", "Port=2222", "-J", "jump", "-i", "key"},
	} {
		parsed, err := parseCLI([]string{"host:job", "--port", "3333"}, "attach")
		if err != nil {
			t.Fatal(err)
		}
		o, err := shellOptions("attach", parsed)
		if err != nil {
			t.Fatal(err)
		}
		applyConnectionOptions(&o, recentConnection{Host: "host", Server: "/custom/sshore", SSH: saved, Directory: t.TempDir()})
		if !reflect.DeepEqual(o.ssh, []string{"-p", "3333", "-J", "jump", "-i", "key"}) || o.server != "/custom/sshore" || o.sshDirectory == "" {
			t.Fatalf("saved connection lost: %+v", o)
		}
	}
}

func TestRoutedShellConnectionRecovery(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	root := options{host: "entry", session: "parent", instance: newTaskUID(), server: "/custom/sshore", ssh: []string{"-p", "2222", "-L", "8080:localhost:80"}, sshDirectory: t.TempDir()}
	middle := options{host: "private-alias", session: "middle", instance: newTaskUID(), routeParent: &root, routeToken: strings.Repeat("a", 64)}
	child := options{host: "private-alias", session: "child", instance: newTaskUID(), routeParent: &middle, routeToken: strings.Repeat("b", 64), routeTarget: true}
	if err := rememberDetachedAttachment(child); err != nil {
		t.Fatal(err)
	}
	if err := rememberConnection(child); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"attach", "private-alias:child"}} {
		o, err := parseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.instance != "" || o.session != "child" || !o.routeTarget || o.routeToken != child.routeToken || o.routeParent == nil || o.routeParent.routeParent == nil {
			t.Fatalf("route not restored: %+v", o)
		}
		restoredRoot := o.routeParent.routeParent
		if restoredRoot.host != root.host || restoredRoot.instance != root.instance || restoredRoot.server != root.server || !reflect.DeepEqual(restoredRoot.ssh, []string{"-p", "2222"}) {
			t.Fatalf("root changed: %+v", restoredRoot)
		}
	}
	dir, _ := stateDir("connections")
	path := filepath.Join(dir, "recent.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// A new daemon revalidates history; the running daemon never reloads live state from files.
		fixture := activityFixture(t)
		if _, err := fixture.metadata.readRecentConnectionsLocal(); err == nil {
			t.Fatal("public route capabilities accepted")
		}
	}
}

func TestRoutedConnectionValidation(t *testing.T) {
	activityFixture(t)
	parent := &recentConnection{Host: "entry", Session: "parent", Server: "sshore", Instance: "parent-instance"}
	r := recentConnection{Host: "private", Session: "job", Server: "sshore", Instance: "child-instance", Parent: parent, RouteToken: strings.Repeat("a", 64)}
	if err := validateRecentConnection(r); err != nil {
		t.Fatal(err)
	}
	r.Parent.Instance = ""
	if err := validateRecentConnection(r); err == nil {
		t.Fatal("unbound parent accepted")
	}
	r.Parent.Instance = "parent-instance"
	r.Parent = &r
	if err := validateRecentConnection(r); err == nil {
		t.Fatal("cyclic route accepted")
	}
}

func TestScopedIDUsesSavedTransport(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	parent := options{host: "entry", session: "parent", instance: strings.Repeat("a", 32)}
	child := options{host: "private", session: "first", instance: strings.Repeat("b", 32), routeParent: &parent, routeToken: strings.Repeat("a", 64)}
	if err := rememberConnection(child); err != nil {
		t.Fatal(err)
	}
	target := options{command: "attach", host: "private", session: "cccccc"}
	if err := resolveRecentAttach(&target); err != nil {
		t.Fatal(err)
	}
	if target.session != "cccccc" || target.instance != "" || !target.routeTarget || target.routeParent == nil || target.routeToken != child.routeToken {
		t.Fatalf("remote selector lost its scope: %+v", target)
	}
}

func TestRoutedParentUsesImmutableRemoteIdentity(t *testing.T) {
	activityFixture(t)
	parent := options{command: "sh", host: "entry", session: "renamed-old-label", instance: strings.Repeat("a", 32)}
	entry := routeParentOptions(parent)
	command := remoteCommand(entry)
	if entry.command != "attach" || entry.session != parent.instance || strings.Contains(command, parent.session) || !strings.Contains(command, parent.instance) {
		t.Fatalf("ancestor startup still depends on its old label: %s", command)
	}
	if parent.session != "renamed-old-label" {
		t.Fatal("display label changed")
	}
	parent.instance = "legacy-instance"
	if got := routeParentOptions(parent); got.session != parent.session {
		t.Fatal("noncanonical instance used as a remote startup target")
	}
}

func TestDetachedMRUPromotionAndPersistence(t *testing.T) {
	fixture := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	register := func(name, id string) *localShell {
		t.Helper()
		shell := &localShell{shellTask: shellTask{
			record:  localShellRecord{Name: name, UID: id},
			process: processIdentity{PID: 1},
			done:    make(chan struct{}),
		}}
		if err := fixture.tasks.register(id, &nativeShellTask{shell: shell}); err != nil {
			t.Fatal(err)
		}
		return shell
	}
	first := register("first", strings.Repeat("a", 32))
	assertTarget := func(want *localShell) {
		t.Helper()
		o := options{local: true}
		if err := resolveDetachedAttach(&o); err != nil || o.taskIdentity != want.record.identity() {
			t.Fatalf("attach selected %q, want %q: %v", o.taskIdentity, want.record.identity(), err)
		}
	}
	assertTarget(first)
	second := register("second", strings.Repeat("b", 32))
	if err := resolveDetachedAttach(&options{local: true}); err == nil || !strings.Contains(err.Error(), "multiple local shells") {
		t.Fatalf("automatic attach selected an unrecorded shell: %v", err)
	}
	for _, shell := range []*localShell{first, second, first} {
		if err := rememberDetachedAttachment(options{local: true, taskIdentity: shell.record.identity(), session: shell.record.Name}); err != nil {
			t.Fatal(err)
		}
	}
	assertTarget(first)

	// Reload the history while keeping both task registrations for the test.
	reloaded := activityFixture(t)
	reloaded.tasks = fixture.tasks
	assertTarget(first)
	first.active = &attachment{}
	assertTarget(second)
}
