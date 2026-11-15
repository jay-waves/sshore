package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

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
