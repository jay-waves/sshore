package main

import (
	"bytes"
	"strings"
	"testing"
)

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
