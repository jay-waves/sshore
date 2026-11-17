package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDetachedMRUScopes(t *testing.T) {
	a, b, c := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	route := func(host, name, id string) *recentConnection {
		return &recentConnection{Host: host, Session: name, Instance: id, Server: "/custom/sshore"}
	}
	rows := []recentAttachment{
		{Identity: "task:" + a, Local: true, Name: "local-job"},
		{Identity: "task:" + b, Name: "remote-job", Connection: route("work", "remote-job", b)},
		{Identity: "task:" + c, Name: "other-job", Connection: route("other", "other-job", c)},
	}
	locals := []localShellView{{identity: "task:" + a, hosted: true, Name: "local-renamed", State: "detached"}}
	snapshots := []shellSnapshot{{Host: "work", Name: "remote-renamed", Instance: b, State: "detached"}, {Host: "other", Name: "other-job", Instance: c, State: "detached"}}
	for _, tc := range []struct {
		name, host string
		want       string
	}{
		{name: "global", want: "local-renamed"},
		{name: "host", host: "work", want: "remote-renamed"},
		{name: "other host", host: "other", want: "other-job"},
		{name: "unknown host", host: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options{command: "attach", host: tc.host, local: false}
			found := selectDetachedAttach(&o, rows, locals, snapshots)
			if found != (tc.want != "") || o.session != tc.want {
				t.Fatalf("found=%v options=%+v", found, o)
			}
			if found && !o.local && o.server != "/custom/sshore" {
				t.Fatal("lost route")
			}
		})
	}
	// A global remote detach becomes first without changing either scoped order.
	rows = []recentAttachment{rows[1], rows[0], rows[2]}
	o := options{command: "attach"}
	if !selectDetachedAttach(&o, rows, locals, snapshots) || o.session != "remote-renamed" {
		t.Fatal(o)
	}
	// Attached, forgotten, uncertain and replaced tasks cannot hide an older entry.
	for _, state := range []string{"attached", "exited", "replaced", "unconfirmed", "forgotten", "active"} {
		t.Run(state, func(t *testing.T) {
			changed := append([]shellSnapshot(nil), snapshots...)
			switch state {
			case "replaced":
				changed[0].Instance = "replacement"
			case "unconfirmed":
				changed[0].Unconfirmed = true
			case "forgotten":
				changed[0].Forgotten = true
			case "active":
				changed[0].Active = true
			default:
				changed[0].State = state
			}
			o := options{command: "attach"}
			if !selectDetachedAttach(&o, rows, locals, changed) || !o.local {
				t.Fatalf("stale entry selected: %+v", o)
			}
		})
	}
}

func TestDetachedMRUPromotionAndPersistence(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	detach := func(name string) {
		t.Helper()
		if err := rememberDetachedAttachment(options{local: true, taskIdentity: "task:" + name, session: name}); err != nil {
			t.Fatal(err)
		}
	}
	detach("first")
	detach("second")
	detach("first")
	reply, err := metadataRPC(metadataRequest{Action: "attach_get"})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, r := range reply.Attachments {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"first", "second"}) {
		t.Fatal(names)
	}
	// Reloading persisted history preserves ordering without promoting an entry.
	accountActivity.Load().metadata.attachmentPath = ""
	rows, err := attachmentMRULocal(nil)
	if err != nil || !reflect.DeepEqual(rows, reply.Attachments) {
		t.Fatalf("reload: %+v %v", rows, err)
	}
}
