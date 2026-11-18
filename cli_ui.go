package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
)

func newLocalInput() (*termui.Input, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, usage("local selection requires an interactive terminal; specify the local endpoint")
	}
	tmux.ResetInputModes()
	if err := tmux.FlushInput(); err != nil {
		return nil, err
	}
	return termui.NewInput()
}

func cleanText(s string) string { return termui.CleanText(s) }

// Session age comes from the daemon lifetime, not the client clock or socket mtime.
// Missing ages (unstarted sessions) have no suffix.
func sessionAge(seconds *int64) string {
	if seconds == nil {
		return ""
	}
	n := max(*seconds, 0)
	for _, unit := range []struct {
		seconds int64
		label   string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if n >= unit.seconds || unit.seconds == 1 {
			return fmt.Sprintf(" · started %d%s ago", n/unit.seconds, unit.label)
		}
	}
	return ""
}

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
	visible := make([]bool, len(names))
	show := func(filter string) {
		matches := 0
		for i := range names {
			visible[i] = strings.Contains(strings.ToLower(labels[i]), strings.ToLower(filter))
			if visible[i] {
				termui.Log(os.Stderr, termui.Info, "  #%d  %s\n", i+1, cleanText(labels[i]))
				matches++
			}
		}
		if matches == 0 {
			termui.Log(os.Stderr, termui.Warning, "No matches. / clears the filter.\n")
		}
	}
	show("")
	for {
		answer, err := input.Read(prompt, false)
		if err != nil {
			return -1, fmt.Errorf("%s selection cancelled: %w", kind, err)
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return -1, fmt.Errorf("%s selection cancelled", kind)
		}
		if strings.HasPrefix(answer, "/") {
			show(strings.TrimSpace(answer[1:]))
			continue
		}
		for i, name := range names {
			if visible[i] && answer == name {
				return i, nil
			}
		}
		n, err := strconv.Atoi(strings.TrimPrefix(answer, "#"))
		if strings.HasPrefix(answer, "#") && err == nil && n >= 1 && n <= len(names) && visible[n-1] {
			return n - 1, nil
		}
		termui.Log(os.Stderr, termui.Warning, "Choose a listed #number or exact identity; /text filters.\n")
	}
}

// Path fields reuse readline editing/completion and retry only local validation.
// No files are created, requests consumed or connections opened here. Review
// fields start with an editable suggestion and still require local submission.
// Control-C/EOF return to the caller with the approval still pending. Transfer
// code independently rechecks paths at execution time (this is only UI feedback).

type promptReader interface {
	Read(string, bool) (string, error)
}

type reviewPromptReader interface {
	ReadPrefilled(string, bool, string) (string, error)
}

func readReviewField(input reviewPromptReader, label string, paths bool, initial string) (string, error) {
	// Terminal control bytes must never enter the readline buffer.
	if cleanText(initial) != initial {
		return "", fmt.Errorf("invalid suggested value; request remains pending")
	}
	line, err := input.ReadPrefilled(label, paths, initial)
	if err != nil {
		return "", fmt.Errorf("approval cancelled; request remains pending: %w", err)
	}
	return line, nil
}

func readApprovalPort(input reviewPromptReader, label, initial string) (int, error) {
	for {
		line, err := readReviewField(input, label+": ", false, initial)
		if err != nil {
			return 0, err
		}
		port, err := parsePort(strings.TrimSpace(line))
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", err)
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
	for {
		line, err := readReviewField(input, prompt, true, initial)
		if err != nil {
			return "", err
		}
		if line == "" && allowEmpty {
			return "", nil
		}
		path := line
		if line == "" || line != initial {
			path, err = termui.ParsePath([]byte(line))
		}
		if err == nil {
			path, err = validate(path)
		}
		if err == nil {
			return path, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", cleanText(err.Error()))
	}
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
			path, err = termui.ParsePath([]byte(line))
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
		termui.Log(os.Stdout, termui.Warning, "%s\n", cleanText(err.Error()))
	}
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
		termui.Log(os.Stdout, termui.Warning, "%s\n", err)
	}
}
