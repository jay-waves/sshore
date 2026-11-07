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
	expected := "`-- 10 bash\n    |-- 20 codex [foreground]\n    |   `-- 21 worker (job) [foreground]\n    `-- 30 sleep\n        `-- 50 bad�name\n"
	if got != expected {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, expected)
	}
	if got := sessionProcessTree(root, 99); !strings.Contains(got, "99 [process unavailable]") {
		t.Fatal(got)
	}
}
