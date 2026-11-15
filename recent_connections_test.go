package main

import (
	"path/filepath"
	"reflect"
	"testing"
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
