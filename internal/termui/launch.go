package termui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/term"
)

// Launch holds the fields displayed and edited before starting a terminal.
type Launch struct {
	Host, Directory, Name    string
	Args                     []string
	Pinned, DefaultShellOnly bool
}

// Review lets the user edit the command and returns whether they confirm launch.
func (launch *Launch) Review() (bool, error) {
	var confirmed bool
	err := RunLocalConsole(func(*LocalConsole) error {
		var reviewErr error
		confirmed, reviewErr = launch.review()
		return reviewErr
	})
	return confirmed, err
}

func (launch *Launch) review() (bool, error) {
	restore, err := term.RawConsole()
	if err != nil {
		return false, err
	}
	defer restore()
	color := ColorEnabled(os.Stdout)
	for {
		if _, err := fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H"); err != nil {
			return false, err
		}
		WriteConsoleBody(os.Stdout, launch.preview(color))
		if _, err := fmt.Fprint(os.Stdout, "[Enter] Start, [e] Edit, [Esc] Cancel"); err != nil {
			return false, err
		}
		key, err := readLaunchKey()
		if err != nil {
			return false, err
		}
		switch key {
		case '\r', '\n':
			return true, nil
		case 0x1b, 0x03:
			return false, nil
		case 'e', 'E':
			if err := launch.edit(); err != nil {
				return false, err
			}
		}
	}
}

func (launch *Launch) preview(color bool) string {
	var body strings.Builder
	body.WriteString(Render(color, Accent, CleanText(launch.Host)))
	branch, _ := TreeBranch(color, "", false)
	if launch.Directory != "" {
		body.WriteString("\n" + branch + "Directory: " + CleanText(launch.Directory))
	}
	if launch.Name != "" {
		body.WriteString("\n" + branch + "Name: " + CleanText(launch.Name))
	}
	if launch.Pinned {
		body.WriteString("\n" + branch + "Keep running after disconnect")
	}
	programBranch, indent := TreeBranch(color, "", true)
	body.WriteString("\n" + programBranch)
	if len(launch.Args) == 0 {
		body.WriteString(Render(color, Heading, "Default shell"))
		return body.String()
	}
	body.WriteString(Render(color, Heading, CleanText(launch.Args[0])))
	args := launch.Args[1:]
	for i, arg := range args {
		branch, _ := TreeBranch(color, indent, i == len(args)-1)
		body.WriteString("\n" + branch + strconv.Quote(arg))
	}
	return body.String()
}

func readLaunchKey() (byte, error) {
	type input struct {
		data []byte
		err  error
	}
	events := make(chan input)
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := term.StartKeyboard(ctx, func(data []byte, err error) {
		select {
		case events <- input{data, err}:
		case <-ctx.Done():
		}
	})
	if err != nil {
		cancel()
		return 0, err
	}
	defer func() {
		cancel()
		stop()
	}()
	var state byte
	var escapeTimeout <-chan time.Time
	for {
		select {
		case <-escapeTimeout:
			return 0x1b, nil
		case event := <-events:
			if event.err != nil {
				return 0, event.err
			}
			escapeTimeout = nil
			for data := event.data; len(data) != 0; {
				key := data[0]
				_, _, n, next := ansi.DecodeSequence(data, state, nil)
				if state == ansi.NormalState && n == 1 {
					switch key {
					case '\r', '\n', 0x03, 'e', 'E':
						return key, nil
					case 0x1b:
						// A lone Escape can also be the start of a split key sequence.
						escapeTimeout = time.After(50 * time.Millisecond)
					}
				}
				data, state = data[max(1, n):], next
			}
		}
	}
}

func (launch *Launch) edit() (err error) {
	input, err := NewInput()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	WriteConsoleBody(os.Stdout, `Quote arguments with spaces. Use "" for an empty argument.
Single quotes preserve text. In double quotes, use \\ for a backslash and \" for a quote.
Leave blank for the default shell. Ctrl+C discards the edit.`)
	draft := shell.FormatCommand(launch.Args)
	for {
		line, err := input.ReadPrefilled("Command> ", false, draft)
		if errors.Is(err, readline.ErrInterrupt) || errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		draft = line
		argv, err := shell.ParseCommand(line)
		switch {
		case err != nil:
			WriteConsoleBody(os.Stdout, err.Error())
		case len(argv) > 0 && launch.DefaultShellOnly:
			WriteConsoleBody(os.Stdout, "Shell startup options require the default shell.")
		default:
			launch.Args = argv
			return nil
		}
	}
}
