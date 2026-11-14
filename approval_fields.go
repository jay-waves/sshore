package main

// Path fields reuse readline editing/completion and retry only local validation.
// No files are created, requests consumed or connections opened here. Blank input
// uses only a displayed default; remote hints still require local submission.
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
	return readSelectedPath(input, prompt, defaultPath, validate, "approval cancelled; request remains pending")
}

func readCopyPath(input promptReader, prompt, defaultPath string, validate func(string) (string, error)) (string, error) {
	return readSelectedPath(input, prompt, defaultPath, validate, "copy cancelled")
}

func readSelectedPath(input promptReader, prompt, defaultPath string, validate func(string) (string, error), cancellation string) (string, error) {
	for {
		line, err := input.Read(prompt, true)
		if err != nil {
			return "", fmt.Errorf("%s: %w", cancellation, err)
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

func readSelectedPort(input promptReader, label, hint, cancellation string) (int, error) {
	if hint != "" {
		label += " [" + hint + "]"
	}
	for {
		value, err := input.Read(label+" (Enter starts; Ctrl-C cancels): ", false)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", cancellation, err)
		}
		if value == "" {
			value = hint
		}
		port, err := parsePort(value)
		if err == nil {
			return port, nil
		}
		logf(os.Stdout, logWarning, "%s\n", err)
	}
}
