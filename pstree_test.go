package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
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
		if !strings.Contains(text, "   ├─ ") || !strings.Contains(text, "   └─ ") || !strings.Contains(text, "← current") || !strings.Contains(text, "cached") {
			t.Fatal(text)
		}
		if strings.Contains(text, "\x1b[") != color {
			t.Fatalf("color=%v: %q", color, text)
		}
	}
	nodes[0].Name = "evil\x1b[2J"
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
	if !strings.Contains(text, "│  └─ remote / spawn/temp") || !strings.Contains(text, "\n└─ remote / launch/job") || !strings.Contains(text, "attach via local / work") {
		t.Fatal(text)
	}
}
