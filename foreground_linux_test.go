package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForegroundSnapshot(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"20": "20 (grep) S 1 42 1",
		"3":  "3 (cat (worker)) S 1 42 1",
		"4":  "4 (background) S 1 99 1",
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
	if got := foregroundPrograms(root, 42); got != "cat (worker) | bad�name | grep" {
		t.Fatal(got)
	}
	for _, group := range []int{0, 100} {
		if got := foregroundPrograms(root, group); got != "—" {
			t.Fatal(got)
		}
	}
	if got := foregroundPrograms(filepath.Join(root, "missing"), 42); got != "—" {
		t.Fatal(got)
	}
}
