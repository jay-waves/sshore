package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellTreeAttachmentSnapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", dir)
	t.Setenv("SSHORE_TREE_ID", "child")
	a := shellTreeNode{Identity: "a", Host: "host-a", Name: "parent"}
	saveShellTree("child", "child", []shellTreeNode{a}, "attached")
	rememberShellTree(shellTreeFor("child"))
	chain := currentShellTree()
	if len(chain) != 2 || chain[1].Parent != "a" || !chain[0].Cached || chain[1].Cached {
		t.Fatalf("initial chain: %+v", chain)
	}
	saveShellTree("child", "child", nil, "detached")
	chain = currentShellTree()
	if len(chain) != 1 || chain[0].Parent != "" || chain[0].State != "detached" {
		t.Fatalf("detach: %+v", chain)
	}
	a.Identity = "b"
	saveShellTree("child", "child", []shellTreeNode{a}, "attached")
	chain = currentShellTree()
	if len(chain) != 2 || chain[1].Parent != "b" {
		t.Fatalf("reattach: %+v", chain)
	}
	renameShellTree("child", "renamed")
	if got := currentShellTree(); got[len(got)-1].Name != "renamed" || got[len(got)-1].Parent != "b" {
		t.Fatalf("rename lost ancestry: %+v", got)
	}
	saveShellTree("child", "child", []shellTreeNode{{Identity: "child"}}, "attached")
	if chain = currentShellTree(); len(chain) != 1 || chain[0].Parent != "" {
		t.Fatalf("cycle accepted: %+v", chain)
	}
	path, _ := shellTreePath("child")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("private snapshot: %v %v", info, err)
	}
}

func TestShellTreeRendering(t *testing.T) {
	nodes := []shellTreeNode{
		{Identity: "p", Host: "alpha", Name: "root"},
		{Identity: "b", Owner: "p", Host: "beta", Name: "one", Cached: true},
		{Identity: "c", Owner: "p", Host: "gamma", Name: "two", Current: true},
	}
	for _, color := range []bool{false, true} {
		var out bytes.Buffer
		writeShellTree(&out, nodes, color)
		text := out.String()
		if !strings.Contains(text, "   ├─ ") || !strings.Contains(text, "   └─ ") || !strings.Contains(text, "*current") || strings.Contains(text, "CHECKED") {
			t.Fatal(text)
		}
		if strings.Contains(text, "HOST  ") || strings.Contains(text, "@") {
			t.Fatal(text)
		}
		if strings.Contains(text, "\x1b[") != color {
			t.Fatalf("color=%v: %q", color, text)
		}
	}
	nodes[0].Name = "evil\x1b[2J"
	nodes[0].ShortID = "bad\x1b[2J"
	nodes[0].Lifetime = "bad\x1b[2J"
	var out bytes.Buffer
	writeShellTree(&out, nodes, false)
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("terminal control injection")
	}
}

func TestShellTreeLifetimeOwnership(t *testing.T) {
	for _, lifetime := range []string{"parent", "heartbeat", "persistent"} {
		t.Run(lifetime, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSHORE_STATE_DIR", dir)
			original := shellTreeNode{Identity: "original", Host: "laptop", Name: "work"}
			saveShellTree("child", "job", []shellTreeNode{original}, "attached", lifetime)
			owner := "original"
			if lifetime == "persistent" {
				owner = ""
			}
			for _, state := range []string{"detached", "attached", "detached", "exited"} {
				var chain []shellTreeNode
				if state == "attached" {
					chain = []shellTreeNode{{Identity: "other", Host: "other", Name: "other"}}
				}
				saveShellTree("child", "job", chain, state, "persistent")
				got := shellTreeFor("child")
				self := got[len(got)-1]
				if self.Owner != owner || self.Lifetime != lifetime || self.State != state {
					t.Fatalf("ownership changed: %+v", got)
				}
				wantParent := ""
				if state == "attached" {
					wantParent = "other"
				}
				if self.Parent != wantParent {
					t.Fatalf("attachment source: %+v", self)
				}
				if owner != "" {
					found := false
					for _, n := range got {
						found = found || n.Identity == owner
					}
					if !found {
						t.Fatalf("owner context lost: %+v", got)
					}
				}
			}
		})
	}
}

func TestShellTreeInventoryRefresh(t *testing.T) {
	t.Setenv("SSHORE_TREE_ID", "remote:instance")
	nodes := inventoryShellTrees([]shellTreeNode{{Identity: "remote:instance", Owner: "owner", Parent: "other", State: "attached", Cached: true}}, inventory{Host: "server", Shells: []shellView{{Instance: "instance", Name: "job", State: "detached", Temporary: true}}})
	if len(nodes) != 1 || nodes[0].State != "detached" || nodes[0].Cached || !nodes[0].Current || nodes[0].Owner != "owner" || nodes[0].Parent != "" || nodes[0].Lifetime != "heartbeat" {
		t.Fatalf("inventory refresh: %+v", nodes)
	}
}

func TestShellTreePersistentAttachmentIsNotOwnership(t *testing.T) {
	var out bytes.Buffer
	writeShellTree(&out, []shellTreeNode{
		{Identity: "origin", Host: "local", Name: "work", Lifetime: "persistent"},
		{Identity: "independent", Parent: "origin", Host: "remote", Name: "job", Lifetime: "persistent"},
		{Identity: "temporary", Owner: "origin", Host: "remote", Name: "temp", State: "detached", Lifetime: "heartbeat"},
	}, false)
	text := out.String()
	if !strings.Contains(text, "(temp)") || !strings.Contains(text, "(job)") || strings.Contains(text, "attach via") {
		t.Fatal(text)
	}
}

func TestShellTreeCompactStates(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint("all_cached=", cached), func(t *testing.T) {
			nodes := []shellTreeNode{
				{Identity: "root", Host: "workstation", Name: "work", Lifetime: "persistent", State: "attached", Cached: cached},
				{Identity: "api", Owner: "root", Host: "dev", Name: "api", Lifetime: "heartbeat", State: "attached", Cached: cached},
				{Identity: "debug", Owner: "api", Host: "dev", Name: "debug", Lifetime: "heartbeat", State: "attached", Current: true, Cached: cached},
				{Identity: "test", Owner: "root", Host: "dev", Name: "test", Lifetime: "heartbeat", State: "detached", Cached: true},
				{Identity: "unknown", Owner: "root", Host: "workstation", Name: "unknown-state", Cached: cached},
			}
			var out bytes.Buffer
			writeShellTree(&out, nodes, false)
			output := out.String()
			if strings.Contains(output, "attached") || strings.Contains(output, "detached") || strings.Contains(output, "CHECKED") || strings.Contains(output, "@dev") {
				t.Fatal(output)
			}
			if strings.Contains(output, "HOST  ") || !strings.Contains(output, "(debug)  *current") {
				t.Fatal(output)
			}

			t.Log("\n" + output)
		})
	}
}

func TestShellTreeCyclesRemainVisible(t *testing.T) {
	var out bytes.Buffer
	writeShellTree(&out, []shellTreeNode{
		{Identity: "a", Owner: "b", Host: "alpha", Name: "cycle-a", State: "detached"},
		{Identity: "b", Owner: "a", Host: "beta", Name: "cycle-b", State: "attached"},
	}, false)
	for _, name := range []string{"cycle-a", "cycle-b"} {
		if strings.Count(out.String(), name) != 1 {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeOmitsEndedOwners(t *testing.T) {
	nodes := inventoryShellTrees([]shellTreeNode{
		{Identity: "gone", Host: "local", Name: "old", State: "exited"},
		{Identity: "child", Owner: "gone", Host: "local", Name: "live", State: "detached", Lifetime: "parent"},
	}, inventory{})
	var out bytes.Buffer
	writeShellTree(&out, nodes, false)
	if strings.Contains(out.String(), "old") || !strings.Contains(out.String(), "(live)") || strings.Contains(out.String(), "owner ended") {
		t.Fatal(out.String())
	}
}

func TestTaskTreeIndependentResources(t *testing.T) {
	root := shellTreeNode{Identity: "root", ShortID: "a0123b", Name: "work", Host: "laptop", Current: true}
	nodes := inventoryResourceTrees([]shellTreeNode{root}, inventory{Resources: []resourceView{{Identity: "mount-id", ShortID: "b0123c", ID: "mount/b0123c", Kind: "mount"}, {Identity: "forward-id", ShortID: "c0123d", ID: "forward/db", Kind: "forward"}}})
	// Force one display host so sibling placement is asserted independently of the fixture computer's name.
	for i := range nodes {
		nodes[i].Host, nodes[i].MachineID = "laptop", ""
	}
	var out bytes.Buffer
	writeShellTree(&out, nodes, false)
	for _, text := range []string{"launch/a0123b (work)  *current", "mount/b0123c", "\n"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "   └─ mount/") || strings.Contains(out.String(), "CHECKED") {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "forward/c0123d (db)") {
		t.Fatal(out.String())
	}
}

func TestLocalTreeScopeAndSources(t *testing.T) {
	machine, foreign := strings.Repeat("a", 32), strings.Repeat("b", 32)
	inv := inventory{MachineID: machine, Hostname: "here", LocalShells: []localShellView{{identity: "local", Name: "local"}}, Shells: []shellView{{Instance: "hosted", State: "attached"}, {Instance: "cached", State: "detached", Historical: true}}}
	nodes := []shellTreeNode{
		{Identity: "foreign", Host: "there", MachineID: foreign},
		{Identity: "local", Name: "local", SourceHost: "here", SourceMachineID: machine, State: "attached"},
		{Identity: "remote:hosted", Name: "hosted", Owner: "foreign", Parent: "foreign", SourceHost: "there", SourceMachineID: foreign, State: "attached"},
		{Identity: "remote:cached", Name: "cached"},
	}
	got := localShellTreeNodes(nodes, inv)
	if len(got) != 2 || got[0].Remote != "" || got[1].Remote != "there" || got[1].Direction != "<-" {
		t.Fatalf("scope/source: %+v", got)
	}
	nodes[2].State = "detached"
	if got := localShellTreeNodes(nodes, inv); got[1].Remote != "" {
		t.Fatalf("stale attachment source: %+v", got)
	}
}

func TestTaskTreeCurrentAndCheckedOrder(t *testing.T) {
	now := time.Now()
	nodes := []shellTreeNode{
		{Identity: "new", ShortID: "b0123c", Kind: "mount", Name: "data", Remote: "server", Direction: "<-", CheckedAt: now},
		{Identity: "parent", ShortID: "a0123b", Name: "work", CheckedAt: now.Add(-time.Hour)},
		{Identity: "current", ShortID: "d0123e", Kind: "spawn", Name: "build", Owner: "parent", Current: true, Remote: "laptop", Direction: "<-", CheckedAt: now.Add(-time.Hour)},
		{Identity: "export", ShortID: "f0123a", Kind: "mount", Name: "export", Remote: "laptop", Direction: "->", CheckedAt: now.Add(-30 * time.Second)},
		{Identity: "old", ShortID: "c0123d", Kind: "forward", Name: "web", Remote: "server", Direction: "->", CheckedAt: now.Add(-time.Minute)},
		{Identity: "unknown", ShortID: "e0123f", Name: "e0123f", State: "unavailable"},
	}
	var out bytes.Buffer
	writeShellTree(&out, nodes, false)
	text := out.String()
	positions := []int{strings.Index(text, "(work)"), strings.Index(text, "*current"), strings.Index(text, "(data)"), strings.Index(text, "(export)"), strings.Index(text, "(web)"), strings.Index(text, "launch/e0123f ?")}
	for i, p := range positions {
		if p < 0 || i > 0 && p <= positions[i-1] {
			t.Fatal(text)
		}
	}
	if strings.Contains(text, "HOST") || strings.Contains(text, "@") {
		t.Fatal(text)
	}
	t.Log("\n" + text)
}

func TestTaskTreeResourceDirections(t *testing.T) {
	inv := inventory{Resources: []resourceView{
		{Identity: "out-mount", Kind: "mount", ShortID: "111111", ID: "mount/data", Host: "server", Worker: "running"},
		{Identity: "out-forward", Kind: "forward", ShortID: "222222", ID: "forward/db", Host: "server", Direction: "<-", Worker: "running"},
		{Identity: "reverse-forward", Kind: "forward", ShortID: "333333", ID: "forward/web", Host: "server", Direction: "->", Worker: "running"},
	}, Peers: []peerView{
		{Identity: "in-mount", ShortID: "444444", ID: "mount/export", Manager: "laptop"},
		{Identity: "in-forward", ShortID: "555555", ID: "forward/service", Direction: "<-", Manager: "laptop"},
		{Identity: "in-reverse", ShortID: "666666", ID: "forward/incoming", Direction: "->", Manager: "laptop"},
	}}
	var out bytes.Buffer
	writeShellTree(&out, inventoryResourceTrees(nil, inv), false)
	for _, line := range []string{"mount/111111 (data)  <- server", "forward/222222 (db)  <- server", "forward/333333 (web)  -> server", "mount/444444 (export)  -> laptop", "forward/555555 (service)  -> laptop", "forward/666666 (incoming)  <- laptop"} {
		if !strings.Contains(out.String(), line) {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeAttachmentSourceSnapshot(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, source := range []string{"first", "second"} {
		saveShellTree("child", "child", nil, "attached", "persistent", source, strings.Repeat("a", 32))
		nodes := shellTreeFor("child")
		if len(nodes) != 1 || nodes[0].SourceHost != source {
			t.Fatalf("source not refreshed: %+v", nodes)
		}
	}
	saveShellTree("child", "child", nil, "detached")
	nodes := shellTreeFor("child")
	if len(nodes) != 1 || nodes[0].SourceHost != "" || nodes[0].SourceMachineID != "" {
		t.Fatalf("ended attachment retained source: %+v", nodes)
	}
}
