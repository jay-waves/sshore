package main

import (
	"bytes"
	"errors"
	"github.com/chzyer/readline"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
	if first["shell:one"].order != 1 || first["mount:two"].order != 2 || second["mount:two"].order != 2 || second["forward:three"].order != 3 {
		t.Fatalf("%v %v", first, second)
	}
	third, err := assignTaskIDs([]string{"shell:one"})
	if err != nil || third["shell:one"].order != 1 {
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
			}, Shells: []shellView{{TaskID: 4, Host: "remote", Name: "work", State: "attached", Historical: true}}})
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
	inv := inventory{Shells: []shellView{
		{Host: "work", Name: "gone", State: "not-listed", Historical: true},
		{Host: "work", Name: "finished", State: "exited", Historical: true},
		{Host: "work", Name: "uncertain", State: "attached", Historical: true, Unconfirmed: true},
		{Host: "work", Name: "active", State: "not-listed", Historical: true, Active: true},
	}}
	var out bytes.Buffer
	writeTaskInventory(&out, inv)
	for _, name := range []string{"gone", "finished", "active"} {
		if strings.Contains(out.String(), "("+name+")") {
			t.Fatal(out.String())
		}
	}
	if !strings.Contains(out.String(), "(uncertain)") {
		t.Fatal(out.String())
	}

}

func TestTaskInventoryCheckedAndColor(t *testing.T) {
	now := time.Now()
	inv := inventory{Shells: []shellView{
		{TaskID: 1, Host: "work", Name: "api", State: "detached", Historical: true, ConfirmedAt: now.Add(-12 * time.Minute)},
		{TaskID: 2, Host: "work", Name: "构建", State: "attached", Historical: true, ConfirmedAt: now.Add(-12 * time.Minute)},
	}}
	var out bytes.Buffer
	writeTaskInventory(&out, inv)
	if !strings.Contains(out.String(), "HOST  work · checked 12m ago") || strings.Contains(out.String(), "CHECKED") || strings.Contains(out.String(), "LIFETIME") || strings.Contains(out.String(), "cached") {
		t.Fatal(out.String())
	}
	inv.Shells[1].ConfirmedAt = now.Add(-2 * time.Hour)
	inv.Shells = append(inv.Shells, shellView{TaskID: 3, Host: "work", Name: "missing", State: "attached", Historical: true})
	inv.LocalShells = []localShellView{{ID: 4, identity: "here", Name: "current", State: "attached"}}
	t.Setenv("SSHORE_LOCAL_SESSION", "here")
	var plain, colored bytes.Buffer
	writeTaskInventoryColor(&plain, inv, false)
	writeTaskInventoryColor(&colored, inv, true)
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
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{
		LocalShells: []localShellView{{ID: 20179, ShortID: "d012ab", Name: "first", State: "detached"}},
		Shells:      []shellView{{TaskID: 183739, ShortID: "a0123b", Host: "remote", Name: "second", State: "attached"}},
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

func TestTaskShortIDCollision(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	const first, second, collision = "fixture:1170", "fixture:4725", "3c7a51"
	if taskShortCode(first, 0) != collision || taskShortCode(second, 0) != collision {
		t.Fatal("collision fixture no longer collides")
	}
	a, err := assignTaskIDs([]string{first})
	if err != nil || a[first].code != collision {
		t.Fatal(a, err)
	}
	// The first task is absent from this list, but its code remains reserved.
	b, err := assignTaskIDs([]string{second})
	if err != nil || b[second].code != taskShortCode(second, 1) || b[second].code == collision {
		t.Fatal(b, err)
	}
	both, err := assignTaskIDs([]string{second, first})
	if err != nil || both[first] != a[first] || both[second] != b[second] {
		t.Fatalf("codes changed when list order changed: %v %v", both, err)
	}
}

func TestCanonicalTaskIDsAndPrefixCollisions(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	first := "abcdef" + strings.Repeat("1", 26)
	collision := "abcdef" + strings.Repeat("2", 26)
	if _, err := assignTaskIDsMode([]string{"task:" + first}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := assignTaskIDsMode([]string{"task:" + collision}, true); !errors.Is(err, errTaskPrefixInUse) {
		t.Fatalf("creation accepted reserved prefix: %v", err)
	}
	// An observation from an independent manager retains its owner's reference.
	refs, err := assignTaskIDs([]string{"task:" + collision, "task:" + first})
	if err != nil || refs["task:"+first].code != "abcdef" || refs["task:"+collision].code != "abcdef" {
		t.Fatalf("import changed references: %v %v", refs, err)
	}
	parsed, err := parseCLI([]string{"abcdef"}, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveShortTask("cancel", &parsed); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous ID accepted: %v", err)
	}
	uid, err := newTaskUID()
	if err != nil || !validTaskUID(uid) || uid[:6] == "abcdef" {
		t.Fatalf("invalid generated identity: %s %v", uid, err)
	}
	refs, err = assignTaskIDs([]string{"task:" + uid})
	if err != nil || refs["task:"+uid].code != uid[:6] {
		t.Fatalf("unstable generated reference: %v %v", refs, err)
	}
}

func TestTaskInventoryIdentityAndType(t *testing.T) {
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
