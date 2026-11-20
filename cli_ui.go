package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

func newLocalInput() (*termui.Input, error) {
	input, err := termui.NewLocalInput()
	if errors.Is(err, termui.ErrTerminalRequired) {
		return nil, usage("%s", err)
	}
	return input, err
}

func cleanText(s string) string { return termui.CleanText(s) }

// Display only the trailing two components of deep directories. Home is shown
// as ~, and a rune limit bounds terminal output without splitting UTF-8.
func shortDirectory(path, home string) string {
	if path == "" {
		return "—"
	}
	path = cleanText(path)
	home = strings.TrimSuffix(cleanText(home), "/")
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 3 || (len(parts) > 2 && parts[0] != "~") {
		path = "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	r := []rune(path)
	if len(r) > 40 {
		path = "…" + string(r[len(r)-39:])
	}
	return path
}

// Session and request selection share the existing line reader, with no second
// input loop or screen ownership. Filters only change visibility; original
// numbers remain stable. Bare input always denotes an exact name;
// #N unambiguously selects an index. Filtering never attaches automatically.
// Setup validates remote names and closes the reader before sending a selection.

func chooseSetupSession(input setupInput, names []string) (string, error) {
	i, err := chooseLocalItem(input, names, names, "Shell (#number/name, /filter; Enter cancels): ", "shell")
	if err != nil {
		return "", err
	}
	return names[i], nil
}

// Return the original row so callers can use the identity attached to it.
func chooseLocalItem(input setupInput, names, labels []string, prompt, kind string) (int, error) {
	return termui.ChooseItem(input, names, labels, prompt, kind)
}

// Path fields reuse readline editing/completion and retry only local validation.
// Fields start with an editable suggestion and require local submission.
// Transfer code independently checks paths at execution time.

type promptReader interface {
	Read(string, bool) (string, error)
}

type prefilledPromptReader interface {
	ReadPrefilled(string, bool, string) (string, error)
}

type reviewPromptReader interface {
	prefilledPromptReader
	ReadPath(string, termui.PathOptions) (string, error)
}

func readReviewField(input prefilledPromptReader, label string, paths bool, initial string) (string, error) {
	// Terminal control bytes must never enter the readline buffer.
	if cleanText(initial) != initial {
		return "", fmt.Errorf("invalid suggested value")
	}
	line, err := input.ReadPrefilled(label, paths, initial)
	if err != nil {
		return "", fmt.Errorf("approval cancelled: %w", err)
	}
	return line, nil
}

func readApprovalPort(input prefilledPromptReader, label, initial string) (int, error) {
	for {
		line, err := readReviewField(input, label+": ", false, initial)
		if err != nil {
			return 0, err
		}
		port, err := parsePort(strings.TrimSpace(line))
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", cleanText(err.Error()))
	}
}

func readApprovalPath(input reviewPromptReader, prompt, defaultPath string, validate func(string) (string, error)) (string, error) {
	return readReviewPath(input, prompt, defaultPath, false, func(path string) (string, error) {
		path, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return validate(path)
	})
}

func readReviewPath(input reviewPromptReader, prompt, initial string, allowEmpty bool, validate func(string) (string, error)) (string, error) {
	path, err := input.ReadPath(prompt, termui.PathOptions{Initial: initial, AllowEmpty: allowEmpty, Validate: validate})
	if err != nil {
		return "", fmt.Errorf("approval cancelled: %w", err)
	}
	return path, nil
}

func readCopyPath(input reviewPromptReader, prompt string, validate func(string) (string, error)) (string, error) {
	path, err := input.ReadPath(prompt, termui.PathOptions{Validate: func(path string) (string, error) {
		path, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return validate(path)
	}})
	if err != nil {
		return "", fmt.Errorf("copy cancelled: %w", err)
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

func readForwardPort(input promptReader, label string) (int, error) {
	for {
		value, err := input.Read(label+" (Enter starts; Ctrl-C cancels): ", false)
		if err != nil {
			return 0, fmt.Errorf("forward cancelled: %w", err)
		}
		port, err := parsePort(value)
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", err)
	}
}
