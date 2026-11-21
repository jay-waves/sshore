package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/termui"
)

func TestShellTreeAttachmentSnapshots(t *testing.T) {
	activityFixture(t)
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
	path, _ := statePath(filepath.Join("shell-tree", shellTreeFilename("child")))
	if info, err := os.Stat(path); err != nil || runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("private snapshot: %v %v", info, err)
	}
}

func TestShellTreeSnapshotSources(t *testing.T) {
	activityFixture(t)
	now := time.Now()
	old := shellTreeNode{Identity: "task", Name: "old", State: "attached", Cached: true, CheckedAt: now.Add(-time.Hour)}
	fresh := old
	fresh.Name, fresh.CheckedAt = "fresh", now
	owner := old
	owner.Name, owner.Cached = "owner", false
	ended := fresh
	ended.Name, ended.State = "ended", "exited"
	for _, tc := range []struct {
		name          string
		first, second shellTreeNode
		want          string
	}{
		{"newer-copy-last", old, fresh, "fresh"},
		{"newer-copy-first", fresh, old, "fresh"},
		{"terminal-copy-after-owner", owner, ended, "ended"},
		{"terminal-copy-before-owner", ended, owner, "ended"},
		{"owner-before-live-copy", owner, fresh, "fresh"},
		{"owner-after-live-copy", fresh, owner, "fresh"},
		{"same-source-terminal-evidence", ended, fresh, "ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			dir, err := stateDir("shell-tree")
			if err != nil {
				t.Fatal(err)
			}
			for i, node := range []shellTreeNode{tc.first, tc.second} {
				path := filepath.Join(dir, fmt.Sprintf("%d.json", i))
				if err := writeStateJSON(path, []shellTreeNode{node}); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil || bytes.Contains(data, []byte("Current")) {
					t.Fatalf("display selection persisted: %s %v", data, err)
				}
			}
			nodes := knownShellTrees()
			if len(nodes) != 1 || nodes[0].Name != tc.want {
				t.Fatalf("wrong snapshot source: %+v", nodes)
			}
		})
	}
}

func TestShellTreeRendering(t *testing.T) {
	activityFixture(t)
	nodes := []shellTreeNode{
		{Identity: "p", Host: "alpha", Name: "root"},
		{Identity: "b", Owner: "p", Host: "beta", Name: "one", Cached: true},
		{Identity: "c", Owner: "p", Host: "gamma", Name: "two"},
	}
	for _, color := range []bool{false, true} {
		var out bytes.Buffer
		writeShellTree(&out, normalizeTaskTree(nodes, ""), "c", color)
		text := out.String()
		lines := strings.Split(strings.TrimSuffix(ansi.Strip(text), "\n"), "\n")
		if len(lines) != 5 || strings.TrimSpace(lines[1]) != "│" || !strings.HasPrefix(lines[2], "├── ") ||
			strings.TrimSpace(lines[3]) != "│" || !strings.HasPrefix(lines[4], "╰── ") ||
			!strings.Contains(text, "*current") || strings.Contains(text, "CHECKED") {
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
	writeShellTree(&out, normalizeTaskTree(nodes, ""), "", false)
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("terminal control injection")
	}
}

func TestShellTreeConsoleStyles(t *testing.T) {
	nodes := []shellTreeNode{{Identity: "root", Name: "work"}, {Identity: "child", Owner: "root", Name: "build"}}
	var plain, styled, console bytes.Buffer
	writeShellTree(&plain, normalizeTaskTree(nodes, ""), "child", false)
	writeShellTree(&styled, normalizeTaskTree(nodes, ""), "child", true)
	if ansi.Strip(styled.String()) != plain.String() {
		t.Fatalf("styles changed tree content: %q", styled.String())
	}
	termui.WriteConsoleBody(&console, styled.String())
	if strings.Contains(console.String(), "�") {
		t.Fatalf("console damaged tree styles: %q", console.String())
	}
}

func TestShellTreeLifetimeOwnership(t *testing.T) {
	activityFixture(t)
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
	activityFixture(t)
	nodes := localShellTreeNodes(inventory{
		Host:      "server",
		ShellTree: []shellTreeNode{{Identity: "remote:instance", Owner: "owner", Parent: "other", State: "attached", Cached: true}},
		Shells:    []shellView{{Instance: "instance", Name: "job", State: "detached", Temporary: true}},
	})
	if len(nodes) != 1 || nodes[0].State != "detached" || nodes[0].Cached || nodes[0].Owner != "owner" || nodes[0].Parent != "" || nodes[0].Lifetime != "heartbeat" {
		t.Fatalf("inventory refresh: %+v", nodes)
	}
}

func TestShellTreePersistentAttachmentIsNotOwnership(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree([]shellTreeNode{
		{Identity: "origin", Host: "local", Name: "work", Lifetime: "persistent"},
		{Identity: "independent", Parent: "origin", Host: "remote", Name: "job", Lifetime: "persistent"},
		{Identity: "temporary", Owner: "origin", Host: "remote", Name: "temp", State: "detached", Lifetime: "heartbeat"},
	}, ""), "", false)
	text := out.String()
	if !strings.Contains(text, "(temp)") || !strings.Contains(text, "(job)") || strings.Contains(text, "attach via") {
		t.Fatal(text)
	}
}

func TestShellTreeCompactStates(t *testing.T) {
	activityFixture(t)
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint("all_cached=", cached), func(t *testing.T) {
			nodes := []shellTreeNode{
				{Identity: "root", Host: "workstation", Name: "work", Lifetime: "persistent", State: "attached", Cached: cached},
				{Identity: "api", Owner: "root", Host: "dev", Name: "api", Lifetime: "heartbeat", State: "attached", Cached: cached},
				{Identity: "debug", Owner: "api", Host: "dev", Name: "debug", Lifetime: "heartbeat", State: "attached", Cached: cached},
				{Identity: "test", Owner: "root", Host: "dev", Name: "test", Lifetime: "heartbeat", State: "detached", Cached: true},
				{Identity: "unknown", Owner: "root", Host: "workstation", Name: "unknown-state", Cached: cached},
			}
			var out bytes.Buffer
			writeShellTree(&out, normalizeTaskTree(nodes, ""), "debug", false)
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
	activityFixture(t)
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree([]shellTreeNode{
		{Identity: "a", Owner: "b", Host: "alpha", Name: "cycle-a", State: "detached"},
		{Identity: "b", Owner: "a", Host: "beta", Name: "cycle-b", State: "attached"},
	}, ""), "", false)
	for _, name := range []string{"cycle-a", "cycle-b"} {
		if strings.Count(out.String(), name) != 1 {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeOmitsEndedOwners(t *testing.T) {
	activityFixture(t)
	nodes := localShellTreeNodes(inventory{
		ShellTree:   []shellTreeNode{{Identity: "gone", Name: "old", State: "exited"}, {Identity: "child", Owner: "gone"}},
		LocalShells: []localShellView{{identity: "child", Name: "live", State: "detached", Temporary: true}},
	})
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(nodes, ""), "", false)
	if strings.Contains(out.String(), "old") || !strings.Contains(out.String(), "(live)") || strings.Contains(out.String(), "owner ended") {
		t.Fatal(out.String())
	}
}

func TestTaskTreeIndependentResources(t *testing.T) {
	activityFixture(t)
	root := shellTreeNode{Identity: "root", ShortID: "a0123b", Name: "work", Host: "laptop"}
	nodes := inventoryResourceTrees([]shellTreeNode{root}, inventory{Resources: []resourceView{{Identity: "mount-id", ShortID: "b0123c", ID: "mount/b0123c", Kind: "mount"}, {Identity: "forward-id", ShortID: "c0123d", ID: "forward/db", Kind: "forward"}}})
	// Force one display host so sibling placement is asserted independently of the fixture computer's name.
	for i := range nodes {
		nodes[i].Host, nodes[i].MachineID = "laptop", ""
	}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(nodes, ""), "root", false)
	for _, text := range []string{"launch/a0123b (work)  *current", "mount/b0123c", "\n"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "╰── mount/") || strings.Contains(out.String(), "CHECKED") {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "forward/c0123d (db)") {
		t.Fatal(out.String())
	}
}

func TestLocalTreeScopeAndSources(t *testing.T) {
	activityFixture(t)
	machine, foreign := strings.Repeat("a", 32), strings.Repeat("b", 32)
	inv := inventory{MachineID: machine, Hostname: "here", LocalShells: []localShellView{{identity: "local", Name: "local", State: "attached"}}, Shells: []shellView{{Instance: "hosted", State: "attached"}}}
	inv.ShellTree = []shellTreeNode{
		{Identity: "foreign", Host: "there", MachineID: foreign},
		{Identity: "local", Name: "local", SourceHost: "here", SourceMachineID: machine, State: "attached"},
		{Identity: "remote:hosted", Name: "hosted", Owner: "foreign", Parent: "foreign", SourceHost: "there", SourceMachineID: foreign, State: "attached"},
		{Identity: "remote:cached", Name: "cached"},
	}
	got := localShellTreeNodes(inv)
	if len(got) != 2 || got[0].Remote != "" || got[1].Remote != "there" || got[1].Direction != "<-" {
		t.Fatalf("scope/source: %+v", got)
	}
	inv.Shells[0].State = "detached"
	if got := localShellTreeNodes(inv); got[1].Remote != "" {
		t.Fatalf("stale attachment source: %+v", got)
	}
}

func TestTaskTreeStableIDOrder(t *testing.T) {
	activityFixture(t)
	now := time.Now()
	nodes := []shellTreeNode{
		{Identity: "new", ShortID: "b0123c", Kind: "mount", Name: "data", Remote: "server", Direction: "<-", CheckedAt: now},
		{Identity: "parent", ShortID: "a0123b", Name: "work", CheckedAt: now.Add(-time.Hour)},
		{Identity: "current", ShortID: "d0123e", Kind: "spawn", Name: "build", Owner: "parent", Remote: "laptop", Direction: "<-", CheckedAt: now.Add(-time.Hour)},
		{Identity: "export", ShortID: "f0123a", Kind: "mount", Name: "export", Remote: "laptop", Direction: "->", CheckedAt: now.Add(-30 * time.Second)},
		{Identity: "old", ShortID: "c0123d", Kind: "forward", Name: "web", Remote: "server", Direction: "->", CheckedAt: now.Add(-time.Minute)},
		{Identity: "unknown", ShortID: "e0123f", Name: "e0123f", State: "unavailable"},
	}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(nodes, ""), "current", false)
	text := out.String()
	positions := []int{strings.Index(text, "(work)"), strings.Index(text, "*current"), strings.Index(text, "(data)"), strings.Index(text, "(web)"), strings.Index(text, "launch/e0123f ?"), strings.Index(text, "(export)")}
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
	activityFixture(t)
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
	writeShellTree(&out, normalizeTaskTree(inventoryResourceTrees(nil, inv), inv.MachineID), "", false)
	for _, line := range []string{"mount/111111 (data)  <- server", "forward/222222 (db)  <- server", "forward/333333 (web)  -> server", "mount/444444 (export)  -> laptop", "forward/555555 (service)  -> laptop", "forward/666666 (incoming)  <- laptop"} {
		if !strings.Contains(out.String(), line) {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeAttachmentSourceSnapshot(t *testing.T) {
	activityFixture(t)
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

func TestExternalSpawnParentDisplay(t *testing.T) {
	start, err := proc.LiveIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	machine := strings.Repeat("a", 32)
	p := &spawnProcessView{PID: os.Getpid(), Start: start, MachineID: machine, Name: "stale-name"}
	input := []shellTreeNode{
		{Identity: "one", Name: "one", Lifetime: "parent", OwnerProcess: p},
		{Identity: "two", Name: "two", Lifetime: "heartbeat", OwnerProcess: p},
		{Identity: "persistent", Lifetime: "persistent", OwnerProcess: p},
	}
	nodes, owners := shellTreeOwners(input, machine)
	if len(nodes) != 4 || owners[0] != 3 || owners[1] != 3 || owners[2] != -1 || input[0].Owner != "" {
		t.Fatalf("bad external ownership: %+v", nodes)
	}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(input, machine), "", false)
	if !strings.Contains(out.String(), fmt.Sprintf("(pid %d)", os.Getpid())) || strings.Contains(out.String(), "external ") || strings.Contains(out.String(), "stale-name") || strings.Contains(out.String(), " ?") {
		t.Fatal(out.String())
	}
	input = append(input, shellTreeNode{Identity: "managed", Lifetime: "persistent"})
	input[0].Owner = "managed"
	_, owners = shellTreeOwners(input, machine)
	if owners[0] != 3 {
		t.Fatal("replaced a visible managed owner")
	}
}

func TestExternalSpawnParentUnavailable(t *testing.T) {
	machine := strings.Repeat("a", 32)
	for _, foreign := range []bool{false, true} {
		p := &spawnProcessView{PID: os.Getpid(), Start: "reused-pid", MachineID: machine, Name: "wrong-process", Host: "other"}
		if foreign {
			p.MachineID = strings.Repeat("b", 32)
		}
		nodes, _ := shellTreeOwners([]shellTreeNode{{Identity: "spawn", Lifetime: "parent", OwnerProcess: p}}, machine)
		if len(nodes) != 2 || nodes[1].State != "unavailable" {
			t.Fatalf("trusted unverified identity: %+v", nodes)
		}
		if foreign && nodes[1].Name != "wrong-process on other" {
			t.Fatal("resolved foreign PID locally")
		}
		if !foreign && nodes[1].Name != "unavailable" {
			t.Fatal("showed a reused PID's name")
		}
	}
}

func TestExternalSpawnOwnerSurvivesReattach(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	activityFixture(t)
	machine := strings.Repeat("a", 32)
	first := &spawnProcessView{PID: 123, Start: "first", MachineID: machine, Name: "bash"}
	second := &spawnProcessView{PID: 456, Start: "second", MachineID: machine, Name: "sh"}
	saveShellTreeWithProcess("external-child", "job", nil, "attached", first, "parent")
	saveShellTree("external-child", "job", nil, "detached")
	saveShellTreeWithProcess("external-child", "job", nil, "attached", second, "parent")
	chain := shellTreeFor("external-child")
	if len(chain) == 0 || chain[len(chain)-1].OwnerProcess == nil || chain[len(chain)-1].OwnerProcess.PID != first.PID {
		t.Fatalf("owner replaced: %+v", chain)
	}
}
