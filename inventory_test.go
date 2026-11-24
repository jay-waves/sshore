package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/termui"
)

func TestInventoryRejectsLegacyStates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	activityFixture(t)
	connections, err := prepareStateDir(root, "connections")
	if err != nil {
		t.Fatal(err)
	}
	old := participatingShell{
		Host: "work", Instance: newTaskUID(), Name: "build", State: "attached",
		PID: 1234, Attachment: newTaskUID(),
	}
	valid := old
	valid.Instance, valid.State = newTaskUID(), taskActive
	if err := writeStateJSON(filepath.Join(connections, "participation.json"), []participatingShell{valid, old}); err != nil {
		t.Fatal(err)
	}
	inv, err := collectInventory()
	if err == nil || len(inv.Shells) != 0 || !strings.Contains(err.Error(), `remote shell history unavailable: entry 2: remote shell has invalid state "attached" (expected active, detached or unknown)`) ||
		!strings.Contains(err.Error(), "participation.json") {
		t.Fatalf("legacy participation diagnosis: %+v, %v", inv.Shells, err)
	}
	trees, err := prepareStateDir(root, "shell-tree")
	if err != nil {
		t.Fatal(err)
	}
	chain := []shellTreeNode{{Identity: "parent", State: "attached"}, {Identity: "child", Owner: "parent", State: taskExited}}
	if err := writeStateJSON(filepath.Join(trees, shellTreeFilename("child")), chain); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"", "attached", "running"} {
		if _, err := metadataRPC(metadataRequest{Action: "tree_save", Key: "job", State: state}); err == nil {
			t.Fatalf("accepted tree_save with state %q", state)
		}
	}
	rows, err := readTaskTree()
	if err == nil || len(rows) != 0 || !strings.Contains(err.Error(), `shell ownership history unavailable: shell tree node 1 has invalid state "attached" (expected starting, active, detached, unknown or exited)`) ||
		!strings.Contains(err.Error(), shellTreeFilename("child")) {
		t.Fatalf("legacy tree diagnosis: %+v, %v", rows, err)
	}
	console := consoleInventoryBody(consoleInventory(message{Name: "pstree"}, "").Output, 9)
	if !strings.Contains(console, `shell tree node 1 has invalid state "attached"`) || strings.Contains(console, "No known tasks") {
		t.Fatalf("console hid the reason: %q", console)
	}
}

func TestInventoryExplainsUnreadableAndInvalidTreeRecords(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"malformed JSON", "[", "unexpected end of JSON input"},
		{"empty tree", "[]", "shell tree has 0 nodes"},
		{"missing identity", `[{"State":"detached"}]`, "shell tree node 1 is missing its task identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			t.Setenv("SSHORE_STATE_DIR", root)
			activityFixture(t)
			dir, err := prepareStateDir(root, "shell-tree")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, shellTreeFilename("job"))
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = readTaskTree()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("tree diagnosis: %v", err)
			}
		})
	}
}

func TestConnectedListUsesLocalHostScope(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	registerTestResource(t, tasks, "forward", "web", strings.Repeat("a", 32))
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = write
	err = showConnectedTasks(options{host: "work"})
	os.Stderr = old
	write.Close()
	output, readErr := io.ReadAll(read)
	read.Close()
	if err != nil || readErr != nil {
		t.Fatalf("list: %v, read: %v", err, readErr)
	}
	if text := string(output); !strings.Contains(text, "web") {
		t.Fatalf("unexpected list: %s", text)
	}
}

func TestExcludeLocalResourcePeers(t *testing.T) {
	local, remote := newTaskUID(), newTaskUID()
	peers := []peerView{{UID: local, ID: "forward/web"}, {UID: remote, ID: "forward/web"}}
	got := excludeLocalResourcePeers(peers, []resourceView{{Identity: "task:" + local}})
	if len(got) != 1 || got[0].UID != remote {
		t.Fatalf("remaining peers: %#v", got)
	}
}

func TestTaskReferencesNeedNoPersistentIndex(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", "relative-invalid-state")
	first, second := taskUID("shell:one"), taskUID("mount:two")
	if !validTaskUID(first) || !validTaskUID(second) || first == second ||
		first != taskUID("shell:one") || second != taskUID("mount:two") {
		t.Fatalf("unstable references: %s %s", first, second)
	}
	uid := newTaskUID()
	if !validTaskUID(uid) {
		t.Fatalf("invalid UID: %s", uid)
	}
}

func TestTaskInventoryCurrentShell(t *testing.T) {
	activityFixture(t)
	for _, current := range []string{"", "local:current", "local:replaced"} {
		t.Run(current, func(t *testing.T) {
			var out bytes.Buffer
			writeTaskInventoryColor(&out, inventory{LocalShells: []localShellView{
				{identity: "local:current", Name: "work", State: taskActive},
				{identity: "local:other", Name: "other", State: taskActive},
				{identity: "local:replaced", Name: "old", State: taskUnknown},
			}, Shells: []shellView{{Host: "remote", Name: "work", State: taskActive}}}, current, false)
			want := 0
			if current == "local:current" {
				want = 1
			}
			if got := strings.Count(out.String(), "* "); got != want {
				t.Fatalf("current marker count=%d want=%d:\n%s", got, want, &out)
			}
			if want == 1 {
				for _, line := range strings.Split(out.String(), "\n") {
					if strings.HasPrefix(line, "* ") && (!strings.Contains(line, "(work)") || !strings.Contains(line, "sh") || !strings.Contains(line, "active")) {
						t.Fatalf("wrong current row: %s", line)
					}
				}
			}
		})
	}
}

func TestTaskInventoryCompactDetails(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	inv := inventory{
		LocalShells: []localShellView{{Name: "work", Directory: "/src/project"}},
		Shells:      []shellView{{Name: "remote", Host: "server", Directory: "/srv/app"}},
		Resources: []resourceView{
			{ID: "mount/data", Detail: "/mnt/data -> /srv/data"},
			{ID: "forward/db", Detail: "localhost:5432 -> server:5432"},
			{ID: "forward/failed", Detail: "mapping", resourceStatus: resourceStatus{LastError: "connection refused"}},
			{ID: "forward/copy", Detail: strings.Repeat("数据", 50) + "\x1b\nend"},
		},
	}
	writeTaskInventory(&out, inv)
	for _, want := range []string{"DETAIL", "/src/project", "/srv/app", "/mnt/data -> /srv/data", "localhost:5432 -> server:5432", "connection refused", strings.Repeat("数据", 50)} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "mapping") || strings.ContainsRune(out.String(), '\x1b') {
		t.Fatal(out.String())
	}
	for _, detail := range []string{"", "short", strings.Repeat("a", 65), strings.Repeat("数", 40), "one\ntwo\x1b"} {
		got := compactTaskDetail(detail, 64)
		if (readline.Runes{}).WidthAll([]rune(got)) > 64 || strings.ContainsAny(got, "\n\x1b") {
			t.Fatalf("invalid detail: %q", got)
		}
	}
}

func TestTaskTableTerminalWidth(t *testing.T) {
	activityFixture(t)
	detail := strings.Repeat("路径/", 40)
	for _, columns := range []int{0, 24, 35, 40, 80, 120} {
		for _, color := range []bool{false, true} {
			table := [][]string{{"ID", "SESSION", "REMOTE", "STATE", "DETAIL"}, {"a0123b", "mount/data", "server", "active", detail}}
			var out bytes.Buffer
			writeTaskTable(&out, table, color, columns, -1)
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(out.String(), "")
			if columns == 0 {
				if !strings.Contains(plain, detail) {
					t.Fatal("redirected detail truncated")
				}
				continue
			}
			for _, line := range strings.Split(strings.TrimSuffix(plain, "\n"), "\n") {
				// The base columns need 35 cells even when DETAIL has no room.
				if (readline.Runes{}).WidthAll([]rune(line)) > max(columns, 35) {
					t.Fatalf("width %d: %q", columns, line)
				}
			}
			if columns > 37 && !strings.Contains(plain, "…") {
				t.Fatalf("width %d: missing ellipsis: %s", columns, plain)
			}
			if !strings.Contains(plain, "a0123b") || !strings.Contains(plain, "active") {
				t.Fatal(plain)
			}
		}
	}
	for _, tc := range []struct {
		detail string
		limit  int
		want   string
	}{
		{"short", 5, "short"}, {"long", 1, "…"}, {"路径", 3, "路…"}, {"路径", 0, ""},
	} {
		if got := compactTaskDetail(tc.detail, tc.limit); got != tc.want {
			t.Fatalf("%+v: %q", tc, got)
		}
	}
}

func TestTaskTableFitsLongDestinationAroundIDAndState(t *testing.T) {
	const id, state = "abcd12", "active"
	destination := strings.Repeat("遠端", 30)
	for _, columns := range []int{42, 0} {
		table := [][]string{{"ID", "TYPE", "REMOTE", "STATE", "DETAIL"}, {id, "sh", destination, state, "-"}}
		var out bytes.Buffer
		writeTaskTable(&out, table, false, columns, 1)
		if !strings.Contains(out.String(), "* "+id) || !strings.Contains(out.String(), state) {
			t.Fatal(out.String())
		}
		if columns == 0 && !strings.Contains(out.String(), destination) {
			t.Fatal("redirected destination was truncated")
		}
		if columns > 0 {
			if !strings.Contains(out.String(), "…") {
				t.Fatal("destination was not shortened")
			}
			for _, line := range strings.Split(out.String(), "\n") {
				if width := (readline.Runes{}).WidthAll([]rune(line)); width > columns {
					t.Fatalf("width %d: %q", width, line)
				}
			}
		}
	}
}

func TestInventoryHostGroups(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	inv := inventory{Host: "server",
		Shells: []shellView{{Name: "build", State: taskDetached}},
		Resources: []resourceView{
			{ID: "mount/local", Host: "server", State: taskUnknown, ReadOnly: true, Detail: "/mnt/local"},
			{ID: "forward/remote", Host: "upstream", State: taskActive},
		},
		Peers: []peerView{{ID: "peer/abcd1234/mount/work", Manager: "laptop", State: taskActive, Detail: "/srv/work"}},
	}
	writeTaskInventory(&out, inv)
	for _, want := range []string{"TYPE", "HOST  local", "REMOTE", "(work)", "laptop", "read-only · /mnt/local", "read-only · /srv/work"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "peer/") {
		t.Fatal("internal peer key in compact view")
	}
	group := ""
	wantGroup := map[string]string{"(build)": "server", "(local)": "local", "(remote)": "local", "(work)": "laptop"}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "HOST  ") {
			group, _, _ = strings.Cut(strings.TrimPrefix(line, "HOST  "), " · ")
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if expected, ok := wantGroup[fields[1]]; ok && group != expected {
			t.Fatalf("%s in %s, want %s", fields[1], group, expected)
		}
		if fields[1] == "(build)" && fields[3] != "-" {
			t.Fatal(line)
		}
		if fields[1] == "(remote)" && fields[3] != "upstream" {
			t.Fatal(line)
		}
		switch fields[1] {
		case "(local)":
			if fields[4] != "unknown" || fields[5] != "read-only" {
				t.Fatal(line)
			}
		case "(remote)", "(work)":
			if fields[4] != "active" {
				t.Fatal(line)
			}
		}
	}
	if !strings.HasPrefix(out.String(), "HOST  local\n") {
		t.Fatal("local group must come first")
	}
}

func TestTaskInventoryColor(t *testing.T) {
	inv := inventory{LocalShells: []localShellView{{identity: "here", Name: "current", State: taskActive}}}
	var plain, colored bytes.Buffer
	writeTaskInventoryColor(&plain, inv, "here", false)
	writeTaskInventoryColor(&colored, inv, "here", true)
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if ansi.ReplaceAllString(colored.String(), "") != plain.String() || !strings.Contains(colored.String(), "\x1b[") {
		t.Fatal("color changed table output")
	}
}

func TestTaskListDisplaysShortIDs(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{
		LocalShells: []localShellView{{ShortID: "d012ab", Name: "first", State: taskDetached}},
		Shells:      []shellView{{ShortID: "a0123b", Host: "remote", Name: "second", State: taskActive}},
	})
	lines := strings.Split(out.String(), "\n")
	for _, row := range []struct{ code, name string }{{"d012ab", "(first)"}, {"a0123b", "(second)"}} {
		found := false
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[0] == row.code && fields[1] == row.name {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %+v in %s", row, &out)
		}
	}
}

func TestCanonicalTaskIDsAndPrefixCollisions(t *testing.T) {
	first := "abcdef" + strings.Repeat("1", 26)
	second := "abcdef" + strings.Repeat("2", 26)
	for _, uid := range []string{first, second} {
		if key := "task:" + uid; taskUID(key) != uid || taskShortCode(key) != "abcdef" {
			t.Fatalf("prefix collision rewrote UID: %s", uid)
		}
	}
}

func TestTaskInventoryIdentityAndType(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{LocalShells: []localShellView{{ShortID: "a0123b", Name: "work", State: taskActive}}, Resources: []resourceView{{ShortID: "b0123c", Kind: "mount", ID: "mount/b0123c", State: taskActive}}})
	for _, text := range []string{"a0123b (work)", "b0123c", "TYPE", "sh", "mount"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "SESSION") || strings.Contains(out.String(), "(b0123c)") {
		t.Fatal(out.String())
	}
}

func TestTaskInventoryCurrentHostedShell(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeTaskInventoryColor(&out, inventory{Shells: []shellView{
		{Name: "nested", Host: "local", Instance: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: taskActive},
	}}, identityFromUID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), false)
	if strings.Count(out.String(), "* ") != 1 || !strings.Contains(out.String(), "sh") || strings.Contains(out.String(), "No tasks") {
		t.Fatal(out.String())
	}
}

func TestTaskTableUsesBorderedLayoutWhenItFits(t *testing.T) {
	for _, color := range []bool{false, true} {
		rows := [][]string{{"ID", "TYPE", "REMOTE", "STATE", "DETAIL"}, {"a0123b", "sh", "work", "active", "/tmp/项目"}}
		var out bytes.Buffer
		writeTaskTable(&out, rows, color, 100, 1)
		var console bytes.Buffer
		termui.WriteConsoleBody(&console, out.String())
		if strings.Contains(console.String(), "�") {
			t.Fatalf("console corrupted styles: %q", console.String())
		}
		plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(out.String(), "")
		for _, want := range []string{"╭", "│ ID", "* a0123b", "active", "/tmp/项目"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("missing %q: %s", want, plain)
			}
		}
		for _, line := range strings.Split(plain, "\n") {
			if (readline.Runes{}).WidthAll([]rune(line)) > 100 {
				t.Fatalf("line too wide: %q", line)
			}
		}
	}
}

func TestShellTreeAttachmentSnapshots(t *testing.T) {
	activityFixture(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", dir)
	t.Setenv("SSHORE_TREE_ID", "child")
	a := shellTreeNode{Identity: "a", Host: "host-a", Name: "parent", State: taskActive}
	saveShellTree("child", "child", []shellTreeNode{a}, taskActive)
	rememberShellTree(shellTreeFor("child"))
	chain := currentShellTree()
	if len(chain) != 2 || chain[1].Parent != "a" || !chain[0].Cached || chain[1].Cached {
		t.Fatalf("initial chain: %+v", chain)
	}
	saveShellTree("child", "child", nil, taskDetached)
	chain = currentShellTree()
	if len(chain) != 1 || chain[0].Parent != "" || chain[0].State != taskDetached {
		t.Fatalf("detach: %+v", chain)
	}
	a.Identity = "b"
	saveShellTree("child", "child", []shellTreeNode{a}, taskActive)
	chain = currentShellTree()
	if len(chain) != 2 || chain[1].Parent != "b" {
		t.Fatalf("reattach: %+v", chain)
	}
	renameShellTree("child", "renamed")
	if got := currentShellTree(); got[len(got)-1].Name != "renamed" || got[len(got)-1].Parent != "b" {
		t.Fatalf("rename lost ancestry: %+v", got)
	}
	saveShellTree("child", "child", []shellTreeNode{{Identity: "child"}}, taskActive)
	if chain = currentShellTree(); len(chain) != 2 || chain[1].Parent != "b" {
		t.Fatalf("cycle accepted: %+v", chain)
	}
	path, _ := statePath(filepath.Join("shell-tree", shellTreeFilename("child")))
	if info, err := os.Stat(path); err != nil || runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("private snapshot: %v %v", info, err)
	}
}

func TestShellTreeSnapshotSources(t *testing.T) {
	fixture := activityFixture(t)
	now := time.Now()
	old := shellTreeNode{Identity: "task", Name: "old", State: taskActive, Cached: true, CheckedAt: now.Add(-time.Hour)}
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
			nodes, err := fixture.metadata.knownShellTrees()
			if err != nil {
				t.Fatal(err)
			}
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
			!strings.Contains(ansi.Strip(text), "(two)  *") || strings.Contains(text, "CHECKED") {
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

func TestShellTreeInlineConnections(t *testing.T) {
	nodes := []shellTreeNode{
		{Identity: "origin", Name: "origin", ShortID: "111111"},
		{Identity: "hosted", Name: "hosted", ShortID: "333333", Remote: "laptop", Direction: "<-"},
	}
	local := []localShellView{{identity: "origin", handoffAttachments: []string{"lease"}}}
	remote := []participatingShell{
		{Host: "work", Instance: strings.Repeat("2", 32), Name: "active", State: taskActive, Attachment: "lease"},
		{Host: "work", Instance: strings.Repeat("4", 32), Name: "other", State: taskActive, Attachment: "other"},
	}
	var out bytes.Buffer
	writeShellTree(&out, addRemoteShells(normalizeTaskTree(nodes, ""), local, remote), "hosted", false)
	text := out.String()
	for _, want := range []string{
		"sh/111111 (origin)  -> work: sh/222222",
		"sh/333333 (hosted)  <- laptop  *",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q from %q", want, text)
		}
	}
	if strings.Contains(text, "444444") {
		t.Fatalf("unrelated remote shell on the same host: %q", text)
	}
	remote[0].State = taskDetached
	remote[0].ChangedAt = time.Now().Add(-49 * time.Hour)
	rows := addRemoteShells(normalizeTaskTree(nodes, ""), local, remote)
	if len(rows) != 3 || len(rows[0].Handoffs) != 0 || rows[2].Code != "222222" || rows[2].Owner != 0 {
		t.Fatalf("detached shell must move from the handoff to the last root: %+v", rows)
	}
	out.Reset()
	writeShellTree(&out, rows, "", false)
	if !strings.HasSuffix(out.String(), "sh/222222 (active)  [detached]  -> work  seen 2 days ago\n") {
		t.Fatal(out.String())
	}
}

func TestShellTreeInventoryRefresh(t *testing.T) {
	activityFixture(t)
	nodes := localShellTreeNodes(inventory{
		Host:      "server",
		ShellTree: []shellTreeNode{{Identity: "task:cccccccccccccccccccccccccccccccc", Owner: "owner", Parent: "other", State: taskActive, Cached: true}},
		Shells:    []shellView{{Instance: "cccccccccccccccccccccccccccccccc", Name: "job", State: taskDetached, Owner: "owner"}},
	})
	if len(nodes) != 1 || nodes[0].State != taskDetached || nodes[0].Cached || nodes[0].Owner != "owner" || nodes[0].Parent != "" {
		t.Fatalf("inventory refresh: %+v", nodes)
	}
}

func TestShellTreePersistentAttachmentIsNotOwnership(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree([]shellTreeNode{
		{Identity: "origin", Host: "local", Name: "work"},
		{Identity: "independent", Parent: "origin", Host: "remote", Name: "job"},
		{Identity: "temporary", Owner: "origin", Host: "remote", Name: "temp", State: taskDetached},
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
				{Identity: "root", Host: "workstation", Name: "work", State: taskActive, Cached: cached},
				{Identity: "api", Owner: "root", Host: "dev", Name: "api", State: taskActive, Cached: cached},
				{Identity: "debug", Owner: "api", Host: "dev", Name: "debug", State: taskActive, Cached: cached},
				{Identity: "test", Owner: "root", Host: "dev", Name: "test", State: taskDetached, Cached: true},
				{Identity: "unknown", Owner: "root", Host: "workstation", Name: "unknown-state", State: taskUnknown, Cached: cached},
			}
			var out bytes.Buffer
			writeShellTree(&out, normalizeTaskTree(nodes, ""), "debug", false)
			output := out.String()
			if strings.Contains(output, "CHECKED") || strings.Contains(output, "@dev") {
				t.Fatal(output)
			}
			if strings.Contains(output, "HOST  ") || !strings.Contains(output, "(debug)  [active]  *") ||
				!strings.Contains(output, "(test)  [detached]") || !strings.Contains(output, "(unknown-state) ?") {
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
		{Identity: "a", Owner: "b", Host: "alpha", Name: "cycle-a", State: taskDetached},
		{Identity: "b", Owner: "a", Host: "beta", Name: "cycle-b", State: taskActive},
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
		ShellTree:   []shellTreeNode{{Identity: "gone", Name: "old", State: taskExited}, {Identity: "child", Owner: "gone"}},
		LocalShells: []localShellView{{identity: "child", Name: "live", State: taskDetached}},
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
	for _, text := range []string{"sh/a0123b (work)  *", "mount/b0123c", "\n"} {
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
	inv := inventory{MachineID: machine, Hostname: "here", LocalShells: []localShellView{{identity: "local", Name: "local", State: taskActive}}, Shells: []shellView{{Instance: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: taskActive}}}
	inv.ShellTree = []shellTreeNode{
		{Identity: "foreign", Host: "there", MachineID: foreign},
		{Identity: "local", Name: "local", SourceHost: "here", SourceMachineID: machine, State: taskActive},
		{Identity: "task:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "hosted", Owner: "foreign", Parent: "foreign", SourceHost: "there", SourceMachineID: foreign, State: taskActive},
		{Identity: "remote:cached", Name: "cached"},
	}
	got := localShellTreeNodes(inv)
	if len(got) != 2 || got[0].Remote != "" || got[1].Remote != "there" || got[1].Direction != "<-" {
		t.Fatalf("scope/source: %+v", got)
	}
	inv.Shells[0].State = taskDetached
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
		{Identity: "current", ShortID: "d0123e", Kind: "sh", Name: "build", Owner: "parent", Remote: "laptop", Direction: "<-", CheckedAt: now.Add(-time.Hour)},
		{Identity: "export", ShortID: "f0123a", Kind: "mount", Name: "export", Remote: "laptop", Direction: "->", CheckedAt: now.Add(-30 * time.Second)},
		{Identity: "old", ShortID: "c0123d", Kind: "forward", Name: "web", Remote: "server", Direction: "->", CheckedAt: now.Add(-time.Minute)},
		{Identity: "unknown", ShortID: "e0123f", Name: "e0123f", State: taskUnknown},
	}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(nodes, ""), "current", false)
	text := out.String()
	positions := []int{strings.Index(text, "(work)"), strings.Index(text, "  *"), strings.Index(text, "(data)"), strings.Index(text, "(web)"), strings.Index(text, "sh/e0123f ?"), strings.Index(text, "(export)")}
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
		{Identity: "out-mount", Kind: "mount", Direction: "<-", ShortID: "111111", ID: "mount/data", Host: "server", State: taskActive},
		{Identity: "out-forward", Kind: "forward", ShortID: "222222", ID: "forward/db", Host: "server", Direction: "<-", State: taskActive},
		{Identity: "reverse-forward", Kind: "forward", ShortID: "333333", ID: "forward/web", Host: "server", Direction: "->", State: taskActive},
	}, Peers: []peerView{
		{UID: newTaskUID(), Identity: "in-mount", ShortID: "444444", ID: "mount/export", Manager: "laptop"},
		{UID: newTaskUID(), Identity: "in-forward", ShortID: "555555", ID: "forward/service", Direction: "<-", Manager: "laptop"},
		{UID: newTaskUID(), Identity: "in-reverse", ShortID: "666666", ID: "forward/incoming", Direction: "->", Manager: "laptop"},
	}}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(inventoryResourceTrees(nil, inv), inv.MachineID), "", false)
	for _, line := range []string{
		"mount/111111 (data)  <- server",
		"forward/222222 (db)  <- server",
		"forward/333333 (web)  -> server",
		"mount/444444 (export)  -> laptop",
		"forward/555555 (service)  -> laptop",
		"forward/666666 (incoming)  <- laptop",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeAttachmentSourceSnapshot(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, source := range []string{"first", "second"} {
		saveShellTree("child", "child", nil, taskActive, source, strings.Repeat("a", 32))
		nodes := shellTreeFor("child")
		if len(nodes) != 1 || nodes[0].SourceHost != source {
			t.Fatalf("source not refreshed: %+v", nodes)
		}
	}
	saveShellTree("child", "child", nil, taskDetached)
	nodes := shellTreeFor("child")
	if len(nodes) != 1 || nodes[0].SourceHost != "" || nodes[0].SourceMachineID != "" {
		t.Fatalf("ended attachment retained source: %+v", nodes)
	}
}
