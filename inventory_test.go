package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/termui"
)

func readTaskTree() ([]taskTreeRow, error) {
	inv, err := collectInventory(true)
	return inv.Tree, err
}

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
	inv, err := collectInventory(false)
	if err == nil || len(inv.Shells) != 0 || !strings.Contains(err.Error(), `remote shell history unavailable: entry 2: remote shell has invalid state "attached" (expected active, waiting, detached or unknown)`) ||
		!strings.Contains(err.Error(), "participation.json") {
		t.Fatalf("legacy participation diagnosis: %+v, %v", inv.Shells, err)
	}
	console := consoleInventoryBody(consoleInventory(message{Name: "pstree"}, "").Output, 9)
	if !strings.Contains(console, `remote shell has invalid state "attached"`) || strings.Contains(console, "No shells.") {
		t.Fatalf("console hid the reason: %q", console)
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
	if !strings.Contains(out.String(), "connection refused · mapping") || strings.ContainsRune(out.String(), '\x1b') {
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
			table := [][]string{{"ID", "TYPE", "STATE", "PARENT", "DETAIL"}, {"a0123b", "mount", "active", "-", detail}}
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
				// The base columns need 29 cells even when DETAIL has no room.
				if (readline.Runes{}).WidthAll([]rune(line)) > max(columns, 29) {
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

func TestTaskTableFitsLongDetailAroundIDAndState(t *testing.T) {
	const id, state, parent = "abcd12", "active", "9abcde"
	destination := strings.Repeat("遠端", 30)
	for _, columns := range []int{42, 0} {
		table := [][]string{{"ID", "TYPE", "STATE", "PARENT", "DETAIL"}, {id, "sh", state, parent, destination}}
		var out bytes.Buffer
		writeTaskTable(&out, table, false, columns, 1)
		if !strings.Contains(out.String(), "* "+id) || !strings.Contains(out.String(), state) || !strings.Contains(out.String(), parent) {
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
			viewRecord(resourceRecord{Kind: "mount", Name: "local", Host: "server", ReadOnly: true, Mountpoint: "/mnt/local", Remote: "/srv/project"}),
			{ID: "forward/remote", Host: "upstream", State: taskActive, Detail: "local:8080 -> upstream:3000"},
		},
		Peers: []peerView{{ID: "peer/abcd1234/mount/work", Manager: "laptop", State: taskActive, Detail: "/srv/work"}},
	}
	writeTaskInventory(&out, inv)
	for _, want := range []string{"TYPE", "HOST  local", "PARENT", "(work)", "laptop", "read-only · /mnt/local -> server:/srv/project", "read-only · /srv/work", "local:8080 -> upstream:3000"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "peer/") || strings.Contains(out.String(), "REMOTE") {
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
		if len(fields) < 6 {
			continue
		}
		if expected, ok := wantGroup[fields[1]]; ok && group != expected {
			t.Fatalf("%s in %s, want %s", fields[1], group, expected)
		}
		switch fields[1] {
		case "(local)":
			if fields[3] != "unknown" || fields[5] != "read-only" {
				t.Fatal(line)
			}
		case "(remote)", "(work)":
			if fields[3] != "active" {
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
		rows := [][]string{{"ID", "TYPE", "STATE", "PARENT", "DETAIL"}, {"a0123b", "sh", "active", "-", "/tmp/项目"}}
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

func TestShellTreeRendering(t *testing.T) {
	activityFixture(t)
	nodes := []shellTreeNode{
		{Identity: "p", Name: "root"},
		{Identity: "b", Owner: "p", Name: "one"},
		{Identity: "c", Owner: "p", Name: "two"},
	}
	for _, color := range []bool{false, true} {
		var out bytes.Buffer
		writeShellTree(&out, normalizeTaskTree(nodes), "c", color)
		text := out.String()
		lines := strings.Split(strings.TrimSuffix(ansi.Strip(text), "\n"), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[1], "├── ") || !strings.HasPrefix(lines[2], "╰── ") ||
			!strings.Contains(ansi.Strip(text), "* two (") || strings.Contains(text, "CHECKED") {
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
	writeShellTree(&out, normalizeTaskTree(nodes), "", false)
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("terminal control injection")
	}
}

func TestShellTreeConsoleStyles(t *testing.T) {
	nodes := []shellTreeNode{{Identity: "root", Name: "work"}, {Identity: "child", Owner: "root", Name: "build"}}
	var plain, styled, console bytes.Buffer
	writeShellTree(&plain, normalizeTaskTree(nodes), "child", false)
	writeShellTree(&styled, normalizeTaskTree(nodes), "child", true)
	if ansi.Strip(styled.String()) != plain.String() {
		t.Fatalf("styles changed tree content: %q", styled.String())
	}
	termui.WriteConsoleBody(&console, styled.String())
	if strings.Contains(console.String(), "�") {
		t.Fatalf("console damaged tree styles: %q", console.String())
	}
}

func TestInventorySeparatesShellTreeAndDetails(t *testing.T) {
	root := identityFromUID(strings.Repeat("1", 32))
	child := identityFromUID(strings.Repeat("2", 32))
	inv := inventory{
		Host: "local",
		LocalShells: []localShellView{
			{identity: root, ShortID: "111111", Name: "dev", State: taskWaiting, Directory: "~/project"},
			{identity: child, ShortID: "222222", Name: "edit", Owner: root, State: taskActive, Directory: "~/project/src"},
		},
		Shells: []shellView{{Host: "gpu", Instance: strings.Repeat("3", 32), Name: "build", State: taskDetached,
			Observed: true, Owner: root, Pinned: true, SeenAt: time.Now().Add(-2 * time.Minute)}},
	}
	for i, f := range []forwardConfig{
		{Name: "db", Host: "gpu", Port: 15432, Target: 5432},
		{Name: "web", Host: "gpu", Port: 8080, Target: 3000, Reverse: true},
	} {
		record := resourceRecord{Kind: "forward", Name: f.Name, Host: f.Host, Direction: f.direction(),
			UID: strings.Repeat(string(rune('4'+i)), 32), Detail: f.description("local", f.Host)}
		view := viewRecord(record)
		view.State = taskActive
		inv.Resources = append(inv.Resources, view)
	}
	inv.setTaskReferences()
	inv.Tree = normalizeTaskTree(localShellTreeNodes(inv))
	var tree, list bytes.Buffer
	writeInventory(&tree, "pstree", inv, child, false, 100)
	writeInventory(&list, "list", inv, child, false, 0)
	const wantTree = "dev (111111)  waiting\n├── * edit (222222)  active\n╰── build (333333) on gpu  detached\n"
	if tree.String() != wantTree {
		t.Fatalf("shell tree: %q", tree.String())
	}
	flat := strings.Join(strings.Fields(list.String()), " ")
	for _, want := range []string{
		"ID TYPE STATE PARENT DETAIL",
		"111111 (dev) sh waiting - ~/project",
		"* 222222 (edit) sh active 111111 ~/project/src",
		"333333 (build) sh detached 111111 pinned on gpu · seen 2 minutes ago",
		"444444 (db) forward active - local:15432 -> gpu:5432",
		"555555 (web) forward active - local:3000 <- gpu:8080",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("missing %q in task list: %s", want, list.String())
		}
	}
}

func TestShellTreePrefersNewestObservationAndLiveRegistration(t *testing.T) {
	now := time.Now()
	uid := newTaskUID()
	old := shellView{Observed: true, Instance: uid, Host: "old", SeenAt: now.Add(-time.Hour)}
	fresh := shellView{Observed: true, Instance: uid, Host: "fresh", SeenAt: now}
	live := shellView{Instance: uid, Name: "registered", State: taskActive}
	for _, views := range [][]shellView{{old, fresh}, {fresh, old}} {
		nodes := localShellTreeNodes(inventory{Shells: views})
		if len(nodes) != 1 || nodes[0].Remote != "fresh" {
			t.Fatalf("latest observation: %+v", nodes)
		}
		for _, withLive := range [][]shellView{append(views, live), append([]shellView{live}, views...)} {
			nodes = localShellTreeNodes(inventory{Shells: withLive})
			if len(nodes) != 1 || nodes[0].Name != "registered" || !nodes[0].SeenAt.IsZero() {
				t.Fatalf("live registration: %+v", nodes)
			}
		}
	}
}

func TestShellTreeShowsUnknownStateWithoutDetails(t *testing.T) {
	owner := identityFromUID(strings.Repeat("a", 32))
	nodes := []shellTreeNode{
		{Identity: "remote", ShortID: "bbbbbb", Owner: owner, Name: "build", Remote: "gpu", Direction: "->", SeenAt: time.Now(), State: taskUnknown},
		{Identity: "native", ShortID: "cccccc", Owner: owner, Name: "dev", Remote: "laptop", Direction: "<-", State: taskActive},
	}
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree(nodes), "", false)
	for _, want := range []string{"build (bbbbbb) on gpu  unknown\n", "dev (cccccc)  active\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
}

func TestShellTreeCyclesRemainVisible(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeShellTree(&out, normalizeTaskTree([]shellTreeNode{
		{Identity: "a", Owner: "b", Name: "cycle-a", State: taskDetached},
		{Identity: "b", Owner: "a", Name: "cycle-b", State: taskActive},
	}), "", false)
	for _, name := range []string{"cycle-a", "cycle-b"} {
		if strings.Count(out.String(), name) != 1 {
			t.Fatal(out.String())
		}
	}
}

func TestInventoryIgnoresObsoleteTreeFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	activityFixture(t)
	dir, err := prepareStateDir(root, "shell-tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unused.json"), []byte("old data"), 0600); err != nil {
		t.Fatal(err)
	}
	if inv, err := collectInventory(false); err != nil || len(inv.Tree) != 0 {
		t.Fatal(inv, err)
	}
}
