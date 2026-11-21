package main

import (
	"bytes"
	"github.com/chzyer/readline"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExcludeLocalResourcePeers(t *testing.T) {
	activityFixture(t)
	manager := taskManagerLabel()
	peers := []peerView{
		{ID: "peer/aaaaaaaa/forward/web", Manager: manager},
		{ID: "peer/bbbbbbbb/forward/web", Manager: manager + "-other"},
		{ID: "peer/cccccccc/mount/web", Manager: manager},
		{ID: "peer/dddddddd/forward/other", Manager: manager},
	}
	got := excludeLocalResourcePeers(peers, []resourceView{{ID: "forward/web"}})
	if len(got) != 3 || got[0].Manager != manager+"-other" || got[1].ID != "peer/cccccccc/mount/web" || got[2].ID != "peer/dddddddd/forward/other" {
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
				{identity: "local:current", Name: "work", State: "attached", Temporary: true},
				{identity: "local:other", Name: "other", State: "attached"},
				{identity: "local:replaced", Name: "old", State: "unavailable"},
			}, Shells: []shellView{{Host: "remote", Name: "work", State: "attached", Historical: true}}}, current, false)
			want := 0
			if current == "local:current" {
				want = 1
			}
			if got := strings.Count(out.String(), "(current)"); got != want {
				t.Fatalf("current marker count=%d want=%d:\n%s", got, want, &out)
			}
			if want == 1 {
				for _, line := range strings.Split(out.String(), "\n") {
					if strings.Contains(line, "(current)") && (!strings.Contains(line, "(work)") || !strings.Contains(line, "spawn") || !strings.Contains(line, "attached (current)")) {
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
			{ID: "review/copy", Detail: strings.Repeat("数据", 50) + "\x1b\nend"},
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
			table := [][]string{{"ID", "SESSION", "REMOTE", "STATE", "DETAIL"}, {"a0123b", "mount/data", "server", "running", detail}}
			var out bytes.Buffer
			writeTaskTable(&out, table, color, columns)
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
			if !strings.Contains(plain, "a0123b") || !strings.Contains(plain, "running") {
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
	const id, state = "abcd12", "attached (current)"
	destination := strings.Repeat("遠端", 30)
	for _, columns := range []int{42, 0} {
		table := [][]string{{"ID", "TYPE", "REMOTE", "STATE", "DETAIL"}, {id, "spawn", destination, state, "-"}}
		var out bytes.Buffer
		writeTaskTable(&out, table, false, columns)
		if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), state) {
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
	inv := inventory{Host: "server", RemoteQueried: true,
		Shells: []shellView{{Name: "build", State: "detached"}},
		Resources: []resourceView{
			{ID: "mount/local", Host: "server", Worker: "running", local: true},
			{ID: "forward/remote", Host: "upstream", Worker: "running"},
		},
		Peers: []peerView{{ID: "peer/abcd1234/mount/work", Manager: "laptop", State: "unknown"}},
	}
	writeTaskInventory(&out, inv)
	for _, want := range []string{"TYPE", "HOST  local", "REMOTE", "(work)", "laptop", "unknown (read-only)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "peer/") {
		t.Fatal("internal peer key in compact view")
	}
	group := ""
	wantGroup := map[string]string{"(build)": "server", "(local)": "local", "(remote)": "server", "(work)": "laptop"}
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
	}
	if !strings.HasPrefix(out.String(), "HOST  local · checked now\n") {
		t.Fatal("local group must come first")
	}
}

func TestTaskInventoryEndedHistory(t *testing.T) {
	activityFixture(t)
	now := time.Now()
	inv := inventory{Shells: []shellView{
		{Host: "work", Name: "gone", State: "detached", Historical: true, Unconfirmed: true, ConfirmedAt: now},
		{Host: "work", Name: "finished", State: "exited", Historical: true},
		{Host: "work", Name: "uncertain", State: "attached", Historical: true, ConfirmedAt: now},
		{Host: "work", Name: "lost-owner", State: "exited", Historical: true, Temporary: true, ConfirmedAt: now},
		{Host: "work", Name: "active", State: "attached", Historical: true, Active: true, Temporary: true, ConfirmedAt: now},
		{Host: "work", Name: "detached", State: "detached", Historical: true, Temporary: true, ConfirmedAt: now},
		{Host: "work", Name: "live", State: "attached", Temporary: true},
	}}
	var out bytes.Buffer
	writeTaskInventory(&out, inv)
	for _, name := range []string{"finished", "lost-owner"} {
		if strings.Contains(out.String(), "("+name+")") {
			t.Fatal(out.String())
		}
	}
	for _, name := range []string{"gone", "uncertain", "active", "detached", "live"} {
		if !strings.Contains(out.String(), "("+name+")") {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "attached (unconfirmed)") || !strings.Contains(out.String(), "unconfirmed") {
		t.Fatal(out.String())
	}
}

func TestTaskInventoryCheckedAndColor(t *testing.T) {
	activityFixture(t)
	now := time.Now()
	inv := inventory{Shells: []shellView{
		{Host: "work", Name: "api", State: "detached", Historical: true, ConfirmedAt: now.Add(-12 * time.Minute)},
		{Host: "work", Name: "构建", State: "attached", Historical: true, ConfirmedAt: now.Add(-12 * time.Minute)},
	}}
	var out bytes.Buffer
	writeTaskInventory(&out, inv)
	if !strings.Contains(out.String(), "HOST  work · checked 12m ago") || strings.Contains(out.String(), "CHECKED") || strings.Contains(out.String(), "LIFETIME") || strings.Contains(out.String(), "cached") {
		t.Fatal(out.String())
	}
	inv.Shells[1].ConfirmedAt = now.Add(-2 * time.Hour)
	inv.Shells = append(inv.Shells, shellView{Host: "work", Name: "missing", State: "attached", Historical: true})
	inv.LocalShells = []localShellView{{identity: "here", Name: "current", State: "attached"}}
	var plain, colored bytes.Buffer
	writeTaskInventoryColor(&plain, inv, "here", false)
	writeTaskInventoryColor(&colored, inv, "here", true)
	for _, want := range []string{"CHECKED", "12m ago", "2h ago", "never", "unknown"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatal(plain.String())
		}
	}
	for _, code := range []string{"\x1b[1m", "\x1b[90m", "\x1b[32m", "\x1b[33m"} {
		if !strings.Contains(colored.String(), code) {
			t.Fatalf("missing style %q: %q", code, colored.String())
		}
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if ansi.ReplaceAllString(colored.String(), "") != plain.String() {
		t.Fatal("color changed table alignment")
	}
	t.Log("\n" + plain.String())
}

func TestShellCheckedAge(t *testing.T) {
	activityFixture(t)
	now := time.Now()
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, "never"}, {now.Add(time.Minute), "unknown"},
		{now, "<1m ago"}, {now.Add(-12 * time.Minute), "12m ago"},
		{now.Add(-2 * time.Hour), "2h ago"}, {now.Add(-48 * time.Hour), "2d ago"},
	} {
		if got := shellCheckedAge(tc.at, now); got != tc.want {
			t.Fatalf("%s != %s", got, tc.want)
		}
	}
}

func TestTaskListDisplaysShortIDs(t *testing.T) {
	activityFixture(t)
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{
		LocalShells: []localShellView{{ShortID: "d012ab", Name: "first", State: "detached"}},
		Shells:      []shellView{{ShortID: "a0123b", Host: "remote", Name: "second", State: "attached"}},
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
	writeTaskInventory(&out, inventory{LocalShells: []localShellView{{ShortID: "a0123b", Name: "work", State: "attached"}}, Resources: []resourceView{{ShortID: "b0123c", Kind: "mount", ID: "mount/b0123c", Worker: "running"}}})
	for _, text := range []string{"a0123b (work)", "b0123c", "TYPE", "launch", "mount"} {
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
		{Name: "nested", Host: "local", Instance: "current", State: "attached", Temporary: true},
		{Name: "cached", Host: "remote", Instance: "current", State: "attached", Historical: true},
	}}, remoteTreeIdentity("current"), false)
	if strings.Count(out.String(), "(current)") != 1 || !strings.Contains(out.String(), "spawn") || strings.Contains(out.String(), "No tasks") {
		t.Fatal(out.String())
	}
}
