package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShellConnectionConfirmation(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	o := options{host: "work", session: "eevee"}
	c, err := trackShellConnection(o)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || !rows[0].Active {
		t.Fatalf("%+v %v", rows, err)
	}
	c.confirm("eevee", "detached")
	c.close()
	rows, err = readShellSnapshots()
	if err != nil || rows[0].Active || rows[0].State != "detached" || rows[0].Unconfirmed {
		t.Fatalf("%+v %v", rows, err)
	}
	v := shellView{State: rows[0].State, ConfirmedAt: rows[0].ConfirmedAt}
	if snapshotState(v) != "-" || snapshotState(shellView{Active: true}) != "local active" || snapshotState(shellView{State: "attached"}) != "unconfirmed" {
		t.Fatal("incorrect cached connection state")
	}
	c, err = trackShellConnection(o)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	rows, err = readShellSnapshots()
	if err != nil || !rows[0].Unconfirmed || rows[0].Active {
		t.Fatalf("%+v %v", rows, err)
	}
	inv := emptyInventory()
	inv.Errors = []string{"unreachable shell"}
	if err = cacheRemoteShells("work", inv); err != nil {
		t.Fatal(err)
	}
	rows, _ = readShellSnapshots()
	if !rows[0].Unconfirmed {
		t.Fatal("incomplete inventory confirmed shell")
	}
	checked := rows[0].ConfirmedAt
	inv.Errors = nil
	if err = cacheRemoteShells("work", inv); err != nil {
		t.Fatal(err)
	}
	rows, _ = readShellSnapshots()
	if !rows[0].Unconfirmed || !rows[0].ConfirmedAt.Equal(checked) {
		t.Fatal(rows)
	}
}

func TestShellHistoryRetention(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	old := time.Now().Add(-shellHistoryRetention - time.Hour)
	for _, state := range []string{"exited", "renamed", "detached", "attached", "unknown"} {
		if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: state, State: state, Unconfirmed: state == "unknown", ConfirmedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rememberConnection(options{host: "work", session: "unknown", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	c, err := trackShellConnection(options{host: "work", session: "active"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "active", State: "exited", ConfirmedAt: old}); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	for _, state := range []string{"unknown", "exited", "renamed"} {
		path, _ := shellCachePath("work", state)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired %s remains: %v", state, err)
		}
	}
	records, err := readRecentConnections()
	if err != nil || len(records) != 1 {
		t.Fatalf("shortcut lost: %+v %v", records, err)
	}
	// A complete query must not refresh tombstones or replace an active connection.
	recent := time.Now().Add(-time.Hour)
	if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "ended", State: "exited", ConfirmedAt: recent}); err != nil {
		t.Fatal(err)
	}
	if err := cacheRemoteShells("work", emptyInventory()); err != nil {
		t.Fatal(err)
	}
	rows, err = readShellSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "ended" && (row.State != "exited" || !row.ConfirmedAt.Equal(recent)) {
			t.Fatalf("history renewed: %+v", row)
		}
		if row.Name == "active" && (row.State != "exited" || !row.Active) {
			t.Fatalf("active record changed: %+v", row)
		}
	}
}

func TestEndedShellCleanup(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, state := range []string{"exited", "finished"} {
		snapshot := shellSnapshot{Host: "work", Name: state, Instance: state, State: "exited", ConfirmedAt: time.Now()}
		if err := saveShellSnapshot(snapshot); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			code, err := runClientOnce(options{host: "work", session: state, command: "cancel"}, "", func() {})
			if code != 0 || err != nil {
				t.Fatalf("named cleanup: %d %v", code, err)
			}
		}
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	for _, s := range rows {
		if !s.Forgotten {
			t.Fatal(s)
		}
	}
	inv, err := collectInventory(options{listJSON: true})
	if err != nil || len(inv.Shells) != 0 {
		t.Fatalf("forgotten history visible: %+v %v", inv.Shells, err)
	}
	if nodes := inventoryShellTrees(knownShellTrees(), inv); len(nodes) != 0 {
		t.Fatal(nodes)
	}
	// A stale ID must never forget a new instance that reused the name.
	if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "exited", Instance: "replacement", State: "detached", ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if cleared, err := forgetEndedShell("work", "exited", "exited"); cleared || err != nil {
		t.Fatalf("replacement cleared: %t %v", cleared, err)
	}
}

func TestShellExitReconcilesHistory(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s := shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "attached", ConfirmedAt: time.Now()}
	if err := saveShellSnapshot(s); err != nil {
		t.Fatal(err)
	}
	saveShellTree("remote:instance", "job", nil, "exited")
	path, err := s.cachePath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := shellTreeFor("remote:instance")
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "exited" {
		t.Fatalf("%+v %v", rows, err)
	}
	if len(evidence) != 1 || !rows[0].ConfirmedAt.Equal(evidence[0].CheckedAt) {
		t.Fatalf("history invented a confirmation time: %+v %+v", rows, evidence)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("reading evidence rewrote cached observation: %v", err)
	}
	if _, err := os.Stat(path + ".live"); !os.IsNotExist(err) {
		t.Fatalf("reading history created a connection lease: %v", err)
	}
	if err := confirmCancelledShell("work", "job", "different"); err != nil {
		t.Fatal(err)
	}
	// Late cached attachment data cannot resurrect an ended instance.
	rememberShellTree([]shellTreeNode{{Identity: "remote:instance", Host: "work", Name: "job", State: "attached"}})
	nodes := inventoryShellTrees(knownShellTrees(), inventory{Shells: []shellView{{Host: "work", Name: "job", Instance: "instance", State: "attached", Historical: true}}})
	if len(nodes) != 0 {
		t.Fatal(nodes)
	}
}

func TestUncachedCancellationRetainsEvidenceTime(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	before := time.Now()
	if err := confirmCancelledShell("work", "job", "instance"); err != nil {
		t.Fatal(err)
	}
	nodes := shellTreeFor("remote:instance")
	if len(nodes) != 1 || nodes[0].State != "exited" || nodes[0].CheckedAt.Before(before) {
		t.Fatalf("cancellation omitted evidence time: %+v", nodes)
	}
}

func TestMissingShellRemainsUnconfirmedWithoutExitEvidence(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	checked := time.Now().Add(-time.Hour)
	s := shellSnapshot{Host: "work", Name: "job", Instance: strings.Repeat("a", 32), State: "detached", ConfirmedAt: checked}
	if err := saveShellSnapshot(s); err != nil {
		t.Fatal(err)
	}
	if err := cacheRemoteShells("work", emptyInventory()); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "detached" || !rows[0].Unconfirmed || !rows[0].ConfirmedAt.Equal(checked) {
		t.Fatalf("absence changed confirmed facts: %+v %v", rows, err)
	}
	if nodes := shellTreeFor(remoteTreeIdentity(s.Instance)); len(nodes) != 0 {
		t.Fatalf("absence created terminal evidence: %+v", nodes)
	}
	if forgotten, err := forgetEndedShell(s.Host, s.Name, s.Instance); forgotten || err != nil {
		t.Fatalf("unconfirmed task treated as exited: %t %v", forgotten, err)
	}
}

func TestRemoteObservationSupersedesAbsence(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", Unconfirmed: true, ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := cacheRemoteShells("work", inventory{Shells: []shellView{{Name: "job", Instance: "instance", State: "detached"}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "detached" || rows[0].Unconfirmed {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestRecentAttachForms(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	original := options{host: "work", session: "eevee", server: "/custom/sshore", sshDirectory: t.TempDir(), ssh: []string{"-p", "2222", "-J", "jump", "-L", "8080:localhost:80"}}
	if err := rememberConnection(original); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"attach"}, {"attach", "work"}, {"attach", "work", "eevee"}} {
		o, err := parseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if err = resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.host != "work" || o.session != "eevee" {
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
func TestRecentAttachMissingAndPerHost(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	empty := options{command: "attach", server: "sshore"}
	if err := resolveRecentAttach(&empty); err == nil {
		t.Fatal("empty recent attach accepted")
	}
	for _, r := range []options{{host: "work", session: "eevee", server: "sshore"}, {host: "other", session: "pikachu", server: "sshore"}, {host: "work", session: "snorlax", server: "sshore"}} {
		if err := rememberConnection(r); err != nil {
			t.Fatal(err)
		}
	}
	for host, want := range map[string]string{"": "snorlax", "work": "snorlax", "other": "pikachu", "unknown": ""} {
		o := options{command: "attach", host: host, server: "sshore"}
		if err := resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.session != want {
			t.Fatalf("%q: %q", host, o.session)
		}
	}
	if err := rememberConnection(options{plain: true, host: "recovery", session: "ignored", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	records, err := readRecentConnections()
	if err != nil || len(records) != 2 {
		t.Fatalf("%+v %v", records, err)
	}
}

func TestRememberShellRenamePreservesHistory(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	original := options{host: "work", session: "old", server: "/custom/sshore", sshDirectory: t.TempDir(), ssh: []string{"-p", "2222"}}
	if err := rememberConnection(original); err != nil {
		t.Fatal(err)
	}
	if err := rememberConnection(options{host: "other", session: "old", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Hour).UTC()
	snapshot := shellSnapshot{Host: "work", Name: "old", State: "detached", ConfirmedAt: when, Unconfirmed: true}
	if err := saveShellSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := rememberShellRename("work", "old", "new"); err != nil {
		t.Fatal(err)
	}
	records, err := readRecentConnections()
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Host != "other" || records[0].Session != "old" || records[1].Host != "work" || records[1].Session != "new" || records[1].Server != original.server || records[1].Directory != original.sshDirectory || !reflect.DeepEqual(records[1].SSH, original.ssh) {
		t.Fatalf("rename changed transport/history: %+v", records)
	}
	o := options{command: "attach", host: "work"}
	if err := resolveRecentAttach(&o); err != nil {
		t.Fatal(err)
	}
	if o.session != "new" {
		t.Fatalf("reattach still uses %q", o.session)
	}
	rows, err := readShellSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range rows {
		if s.Host == "work" && s.Name == "new" {
			found = true
			if s.State != snapshot.State || s.Unconfirmed != snapshot.Unconfirmed || !s.ConfirmedAt.Equal(when) {
				t.Fatalf("rename fabricated confirmation: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("renamed shell snapshot missing")
	}
}

func TestNativePortOverridesSavedConnection(t *testing.T) {
	activityFixture(t)
	for _, saved := range [][]string{
		{"-p", "2222", "-J", "jump", "-i", "key"},
		{"-o", "Port=2222", "-J", "jump", "-i", "key"},
	} {
		parsed, err := parseCLI([]string{"host", "job", "--port", "3333"}, "attach")
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
	root := options{host: "entry", session: "parent", instance: "parent-instance", server: "/custom/sshore", ssh: []string{"-p", "2222", "-L", "8080:localhost:80"}, sshDirectory: t.TempDir()}
	middle := options{host: "private-alias", session: "middle", instance: "middle-instance", routeParent: &root, routeToken: strings.Repeat("a", 64)}
	child := options{host: "private-alias", session: "child", instance: "child-instance", routeParent: &middle, routeToken: strings.Repeat("b", 64), routeTarget: true}
	c, err := trackShellConnection(child)
	if err != nil {
		t.Fatal(err)
	}
	c.confirm("child", "detached")
	c.close()
	if err := rememberConnection(child); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"attach"}, {"attach", "private-alias", "child"}} {
		o, err := parseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolveRecentAttach(&o); err != nil {
			t.Fatal(err)
		}
		if o.instance != child.instance || !o.routeTarget || o.routeToken != child.routeToken || o.routeParent == nil || o.routeParent.routeParent == nil {
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
		activityFixture(t)
		if _, err := readRecentConnections(); err == nil {
			t.Fatal("public route capabilities accepted")
		}
	}
}

func TestRoutedShellCacheSeparatesAliases(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, suffix := range []string{"one", "two"} {
		parent := options{host: "entry-" + suffix, session: "parent", instance: "parent-" + suffix}
		o := options{host: "private", session: "job", instance: "child-" + suffix, routeParent: &parent, routeToken: strings.Repeat("a", 64)}
		c, err := trackShellConnection(o)
		if err != nil {
			t.Fatal(err)
		}
		c.confirm("job", "detached")
		c.close()
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 2 {
		t.Fatalf("routes collided: %+v, %v", rows, err)
	}
	o := options{command: "attach", host: "private", session: "job"}
	if err := resolveRecentAttach(&o); err == nil {
		t.Fatal("ambiguous alias selected")
	}
	o.instance = "child-one"
	if err := resolveRecentAttach(&o); err != nil {
		t.Fatal(err)
	}
	if o.routeParent == nil || o.routeParent.host != "entry-one" {
		t.Fatalf("wrong route: %+v", o)
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

func TestRoutedInventoryKeepsSiblingDestinations(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	parent := options{host: "entry", session: "parent", instance: "parent-instance"}
	o := options{host: "private", session: "first", instance: "first-instance", routeParent: &parent, routeToken: strings.Repeat("a", 64)}
	inv := inventory{Shells: []shellView{{Name: "first", Instance: "first-instance", State: "attached"}, {Name: "sibling", Instance: "sibling-instance", State: "detached"}}}
	if err := cacheRemoteShells(o.host, inv, o); err != nil {
		t.Fatal(err)
	}
	restored := options{command: "attach", host: "private", session: "sibling"}
	if err := resolveRecentAttach(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored.routeTarget || restored.routeParent == nil || restored.instance != "sibling-instance" || restored.routeToken != o.routeToken {
		t.Fatalf("sibling lost destination: %+v", restored)
	}
}

func TestRoutedParentUsesImmutableBootstrapIdentity(t *testing.T) {
	activityFixture(t)
	parent := options{command: "spawn", host: "entry", session: "renamed-old-label", instance: strings.Repeat("a", 32)}
	entry := routeParentOptions(parent)
	command := remoteCommand(entry)
	if entry.command != "attach" || entry.session != parent.instance || strings.Contains(command, parent.session) || !strings.Contains(command, parent.instance) {
		t.Fatalf("ancestor bootstrap still depends on its old label: %s", command)
	}
	if parent.session != "renamed-old-label" {
		t.Fatal("display label changed")
	}
	parent.instance = "legacy-instance"
	if got := routeParentOptions(parent); got.session != parent.session {
		t.Fatal("noncanonical instance used as a bootstrap target")
	}
}

func TestRoutedInventoryExpiryIsScoped(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	var first options
	for _, suffix := range []string{"one", "two"} {
		parent := options{host: "entry-" + suffix, session: "parent", instance: "parent-" + suffix}
		o := options{host: "private", session: "job", instance: "child-" + suffix, routeParent: &parent, routeToken: strings.Repeat("a", 64)}
		if suffix == "one" {
			first = o
		}
		if err := cacheRemoteShells(o.host, inventory{Shells: []shellView{{Name: "job", Instance: o.instance, State: "detached"}}}, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := cacheRemoteShells("private", inventory{Shells: []shellView{{Name: "job", Instance: "replacement-instance", State: "detached"}}}, first); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	uncertain := map[string]bool{}
	for _, row := range rows {
		states[row.Instance] = row.State
		uncertain[row.Instance] = row.Unconfirmed
	}
	if !uncertain["child-one"] || uncertain["child-two"] || states["child-two"] != "detached" || states["replacement-instance"] != "detached" {
		t.Fatalf("wrong scope expired: %+v", states)
	}
}
