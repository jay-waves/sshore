package proc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestReadProcesses(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"20": "20 (grep) S 1 42 1",
		"3":  "3 (cat (worker)) S 1 42 1 1 42",
		"4":  "4 (background) S 1 99 1 1 invalid",
		"5":  "malformed",
		"6":  "6 (bad\nname) S 1 42 1",
	} {
		dir := filepath.Join(root, pid)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want := []Process{
		{PID: 3, Parent: 1, Group: 42, Foreground: 42, Name: "cat (worker)"},
		{PID: 4, Parent: -1, Group: 99, Foreground: -1, Name: "background"},
		{PID: 6, Parent: -1, Group: 42, Foreground: -1, Name: "bad\nname"},
		{PID: 20, Parent: -1, Group: 42, Foreground: -1, Name: "grep"},
	}
	if got := readProcesses(root); !slices.Equal(got, want) {
		t.Fatalf("readProcesses() = %+v, want %+v", got, want)
	}
	if got := readProcesses(filepath.Join(root, "missing")); len(got) != 0 {
		t.Fatalf("readProcesses(missing) = %+v, want no processes", got)
	}
}
