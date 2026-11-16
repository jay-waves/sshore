package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSpawnOptionsAndInventory(t *testing.T) {
	for _, args := range [][]string{{"spawn"}, {"spawn", "--local", "dev", "--detach"}, {"spawn", "host", "dev", "--", "-p", "2222"}} {
		o, err := parseOptions(args)
		if err != nil || o.command != "spawn" {
			t.Fatalf("%v: %+v %v", args, o, err)
		}
	}
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{LocalShells: []localShellView{{ID: 1, Name: "persistent", State: "detached"}, {ID: 2, Name: "temporary", State: "detached", Temporary: true}}, Shells: []shellView{{TaskID: 3, Name: "remote", Host: "host", State: "detached", Temporary: true, Historical: true}}}, false)
	for _, want := range []string{"launch/persistent", "spawn/temporary", "spawn/remote", "cached"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "MODE") {
		t.Fatal("unexpected extra mode column")
	}
}
