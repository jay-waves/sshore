package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcludeLocalResourcePeers(t *testing.T) {
	manager := taskManagerLabel()
	peers := []peerView{
		{ID: "peer/aaaaaaaa/forward/web", Manager: manager},
		{ID: "peer/bbbbbbbb/forward/web", Manager: manager + "-other"},
		{ID: "peer/cccccccc/mount/web", Manager: manager},
		{ID: "peer/dddddddd/forward/other", Manager: manager},
	}
	got := excludeLocalResourcePeers(peers, []resourceRecord{{Kind: "forward", Name: "web"}})
	if len(got) != 3 || got[0].Manager != manager+"-other" || got[1].ID != "peer/cccccccc/mount/web" || got[2].ID != "peer/dddddddd/forward/other" {
		t.Fatalf("remaining peers: %#v", got)
	}
}

func TestTaskIDsPersistWithoutReorderingOrReuse(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	first, err := assignTaskIDs([]string{"shell:one", "mount:two"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := assignTaskIDs([]string{"mount:two", "forward:three"})
	if err != nil {
		t.Fatal(err)
	}
	if first["shell:one"] != 1 || first["mount:two"] != 2 || second["mount:two"] != 2 || second["forward:three"] != 3 {
		t.Fatalf("%v %v", first, second)
	}
	third, err := assignTaskIDs([]string{"shell:one"})
	if err != nil || third["shell:one"] != 1 {
		t.Fatalf("%v %v", third, err)
	}
}

func TestTaskInventoryCurrentShell(t *testing.T) {
	for _, current := range []string{"", "local:current", "local:replaced"} {
		t.Run(current, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", current)
			var out bytes.Buffer
			writeTaskInventory(&out, inventory{LocalShells: []localShellView{
				{ID: 1, identity: "local:current", Name: "work", State: "attached", Temporary: true},
				{ID: 2, identity: "local:other", Name: "other", State: "attached"},
				{ID: 3, identity: "local:replaced", Name: "old", State: "unavailable"},
			}, Shells: []shellView{{TaskID: 4, Host: "remote", Name: "work", State: "attached", Historical: true}}}, false)
			want := 0
			if current == "local:current" {
				want = 1
			}
			if got := strings.Count(out.String(), "(current)"); got != want {
				t.Fatalf("current marker count=%d want=%d:\n%s", got, want, &out)
			}
			if want == 1 {
				for _, line := range strings.Split(out.String(), "\n") {
					if strings.Contains(line, "(current)") && (!strings.Contains(line, "spawn/work") || !strings.Contains(line, "attached (current)")) {
						t.Fatalf("wrong current row: %s", line)
					}
				}
			}
		})
	}
}

func TestTaskInventoryProcessTreeLines(t *testing.T) {
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{Shells: []shellView{{Name: "work", ProcessTree: "bash (1)\n└─ bad\x1b[31m (2)\n"}}}, true)
	if !strings.Contains(out.String(), "launch/work\nbash (1)\n└─ bad�[31m (2)\n") {
		t.Fatalf("process tree formatting: %q", out.String())
	}
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Fatal("terminal escape leaked")
	}
}

func TestInventoryHostGroups(t *testing.T) {
	var out bytes.Buffer
	inv := inventory{Host: "server", RemoteQueried: true,
		Shells: []shellView{{TaskID: 1, Name: "build", State: "detached"}},
		Resources: []resourceView{
			{TaskID: 2, ID: "mount/local", Host: "server", Worker: "running", local: true},
			{TaskID: 3, ID: "forward/remote", Host: "upstream", Worker: "running"},
		},
		Peers: []peerView{{TaskID: 4, ID: "peer/abcd1234/mount/work", Manager: "laptop", State: "unknown"}},
	}
	writeTaskInventory(&out, inv, false)
	for _, want := range []string{"SESSION", "HOST · local", "REMOTE", "mount/work", "laptop", "unknown (read-only)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "peer/") {
		t.Fatal("internal peer key in compact view")
	}
	group := ""
	wantGroup := map[string]string{"launch/build": "server", "mount/local": "local", "forward/remote": "server", "mount/work": "laptop"}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "HOST · ") {
			group = strings.TrimPrefix(line, "HOST · ")
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if expected, ok := wantGroup[fields[1]]; ok && group != expected {
			t.Fatalf("%s in %s, want %s", fields[1], group, expected)
		}
		if fields[1] == "launch/build" && fields[2] != "-" {
			t.Fatal(line)
		}
		if fields[1] == "forward/remote" && fields[2] != "upstream" {
			t.Fatal(line)
		}
	}
	if !strings.HasPrefix(out.String(), "HOST · local\n") {
		t.Fatal("local group must come first")
	}
	out.Reset()
	writeTaskInventory(&out, inv, true)
	if !strings.Contains(out.String(), "peer/abcd1234/mount/work") {
		t.Fatal("detail lost peer identity")
	}
}
