package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestConsoleCopyPathCompletion(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"前缀-甲.txt", "前缀-乙 二.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	line := "cp self:" + filepath.Join(dir, "前缀-")
	candidates, _ := (consoleCompleter(func() []sessionTarget { return nil })).Do([]rune(line), len([]rune(line)))
	if len(candidates) != 2 {
		t.Fatalf("candidates: %q", candidates)
	}
	var paths []string
	for _, suffix := range candidates {
		args, err := consoleResourceArgs(line + string(suffix))
		if err != nil || len(args) != 2 {
			t.Fatalf("completed arguments %q: %v", args, err)
		}
		paths = append(paths, args[1])
	}
	for _, name := range []string{"前缀-甲.txt", "前缀-乙 二.txt"} {
		if !slices.Contains(paths, "self:"+filepath.Join(dir, name)) {
			t.Fatalf("missing %q in %q", name, paths)
		}
	}
}
