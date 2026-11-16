package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestShellConnectionConfirmation(t *testing.T) {
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
	if rows[0].State == "not-listed" {
		t.Fatal("incomplete inventory erased shell")
	}
	inv.Errors = nil
	if err = cacheRemoteShells("work", inv); err != nil {
		t.Fatal(err)
	}
	rows, _ = readShellSnapshots()
	if rows[0].State != "not-listed" || rows[0].Unconfirmed {
		t.Fatal(rows)
	}
}

func TestShellHistoryRetention(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	old := time.Now().Add(-shellHistoryRetention - time.Hour)
	for _, state := range []string{"not-listed", "exited", "renamed", "detached", "attached", "unknown"} {
		if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: state, State: state, ConfirmedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rememberConnection(options{host: "work", session: "not-listed", server: "sshore"}); err != nil {
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
	if len(rows) != 4 {
		t.Fatalf("%+v", rows)
	}
	for _, state := range []string{"not-listed", "exited", "renamed"} {
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
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, state := range []string{"exited", "not-listed"} {
		snapshot := shellSnapshot{Host: "work", Name: state, Instance: state, State: state, ConfirmedAt: time.Now()}
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
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s := shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "attached", ConfirmedAt: time.Now()}
	if err := saveShellSnapshot(s); err != nil {
		t.Fatal(err)
	}
	saveShellTree("remote:instance", "job", nil, "exited")
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "exited" {
		t.Fatalf("%+v %v", rows, err)
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

func TestRemoteObservationSupersedesAbsence(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if err := saveShellSnapshot(shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "not-listed", ConfirmedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := cacheRemoteShells("work", inventory{Shells: []shellView{{Name: "job", Instance: "instance", State: "detached"}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || rows[0].State != "detached" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestRecentAttachForms(t *testing.T) {
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
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	original := options{host: "work", session: "old", server: "/custom/sshore", sshDirectory: t.TempDir(), ssh: []string{"-p", "2222"}}
	if err := rememberConnection(original); err != nil {
		t.Fatal(err)
	}
	if err := rememberConnection(options{host: "other", session: "old", server: "sshore"}); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
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
