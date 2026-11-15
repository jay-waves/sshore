package main

import (
	"path/filepath"
	"testing"
)

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
