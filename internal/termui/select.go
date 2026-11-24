package termui

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jay-waves/sshore/internal/pty"
	"golang.org/x/term"
)

var ErrTerminalRequired = errors.New("local selection requires an interactive terminal; specify the local endpoint")

// NewLocalInput obtains a local terminal after clearing application input modes.
func NewLocalInput() (*Input, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, ErrTerminalRequired
	}
	pty.ResetInputModes()
	if err := pty.FlushInput(); err != nil {
		return nil, err
	}
	return NewInput()
}

// ItemReader supplies input for a filtered local selection.
type ItemReader interface {
	Read(string, bool) (string, error)
}

func ChooseItem(input ItemReader, names, labels []string, prompt, kind string) (int, error) {
	color := ColorEnabled(os.Stderr)
	visible := make([]bool, len(names))
	show := func(filter string) {
		filter = strings.ToLower(filter)
		matches := 0
		for i := range names {
			visible[i] = strings.Contains(strings.ToLower(labels[i]), filter)
			if visible[i] {
				id := Render(color, Accent, fmt.Sprintf("#%d", i+1))
				fmt.Fprintf(os.Stderr, "  %s  %s\n", id, CleanText(labels[i]))
				matches++
			}
		}
		if matches == 0 {
			fmt.Fprintln(os.Stderr, StatusLine(color, Warning, "No matches. / clears the filter."))
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
		fmt.Fprintln(os.Stderr, StatusLine(color, Warning, "Choose a listed #number or exact identity; /text filters."))
	}
}

// PathOptions controls local path selection. Validate applies the callers'
// endpoint-specific rules after parsing.
type PathOptions struct {
	Initial    string
	AllowEmpty bool
	Validate   func(string) (string, error)
}

type prefilledReader interface {
	ReadPrefilled(string, bool, string) (string, error)
}

// ReadPath reads and validates a local path using the supplied line editor.
func ReadPath(input prefilledReader, prompt string, options PathOptions) (string, error) {
	if CleanText(options.Initial) != options.Initial {
		return "", fmt.Errorf("invalid suggested value")
	}
	for {
		line, err := input.ReadPrefilled(prompt, true, options.Initial)
		if err != nil {
			return "", err
		}
		if line == "" && options.AllowEmpty {
			return "", nil
		}
		path := line
		if line == "" || line != options.Initial {
			path, err = ParsePath([]byte(line))
		}
		if err == nil {
			path, err = options.Validate(path)
		}
		if err == nil {
			return path, nil
		}
		Log(os.Stdout, "", Warning, "%s\n", CleanText(err.Error()))
	}
}

// ReadPath prompts for a local path with editing and filename completion.
func (p *Input) ReadPath(prompt string, options PathOptions) (string, error) {
	return ReadPath(p, prompt, options)
}
