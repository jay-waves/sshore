package main

// Path fields reuse readline editing/completion and retry only local validation.
// No files are created, requests consumed or connections opened here. Blank input
// uses an explicit download default; uploads never acquire an implicit source.
// Control-C/EOF return to the caller with the approval still pending. Transfer
// code independently rechecks paths at execution time (this is only UI feedback).

import (
	"fmt"
	"os"
	"path/filepath"
)

type promptReader interface {
	Read(string, bool) (string, error)
}

func readApprovalPath(input promptReader, prompt, defaultPath string, validate func(string) (string, error)) (string, error) {
	for {
		line, err := input.Read(prompt, true)
		if err != nil {
			return "", fmt.Errorf("approval cancelled; request remains pending: %w", err)
		}
		path := defaultPath
		if line != "" || path == "" {
			path, err = localPath([]byte(line))
		}
		if err == nil {
			path, err = filepath.Abs(path)
		}
		if err == nil {
			path, err = validate(path)
		}
		if err == nil {
			return path, nil
		}
		logf(os.Stdout, logWarning, "%s\n", cleanText(err.Error()))
	}
}

func downloadDestination(path, name string) (string, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, name)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("destination already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("destination parent is not a directory")
	}
	return path, nil
}

func displayBytes(size int64) string {
	for _, unit := range []struct {
		size  int64
		label string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if size >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(size)/float64(unit.size), unit.label)
		}
	}
	return fmt.Sprintf("%d B", size)
}
