//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionProcessTree(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"10": "10 (bash) S 1 10 10 1 20",
		"20": "20 (codex) S 10 20 10 1 20",
		"21": "21 (worker (job)) S 20 20 10 1 20",
		"30": "30 (sleep) S 10 30 10 1 20",
		"40": "40 (unrelated) S 1 40 40 1 40",
		"50": "50 (bad\x1bname) S 30 30 10 1 20",
		"60": "invalid",
	} {
		path := filepath.Join(root, pid)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := sessionProcessTree(root, 10)
	expected := "bash (10)\n├─ codex (20) [fg]\n│  └─ worker (job) (21) [fg]\n└─ sleep (30)\n   └─ bad�name (50)\n"
	if got != expected {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, expected)
	}
	if got := sessionProcessTree(root, 99); !strings.Contains(got, "99 [process unavailable]") {
		t.Fatal(got)
	}
}

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
