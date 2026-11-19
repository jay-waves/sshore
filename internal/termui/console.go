package termui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/chzyer/readline"
)

// Console edits commands using bytes supplied by the terminal's input owner.
type Console struct {
	ctx     context.Context
	cancel  context.CancelFunc
	input   chan []byte
	prompts chan consolePrompt
}

type consolePrompt struct {
	body      string
	completer readline.AutoCompleter
}

type ConsoleEvent struct {
	Output []byte
	Line   *string
	Closed bool
}

type consoleWriter func([]byte)

func (w consoleWriter) Write(p []byte) (int, error) {
	w(bytes.Clone(p))
	return len(p), nil
}

// DrawConsole clears the current screen and draws a plain text title and body.
func DrawConsole(out io.Writer, title, body string) {
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = CleanText(lines[i])
	}
	fmt.Fprintf(out, "\x1b[2J\x1b[H%s\r\n\r\n%s\r\n\r\n", CleanText(title), strings.Join(lines, "\r\n"))
}

// StartConsole starts a Readline editor. The caller continues to own the
// terminal modes and serializes emitted output with its other terminal writes.
func StartConsole(ctx context.Context, title, prompt string, width int, send func(ConsoleEvent)) *Console {
	ctx, cancel := context.WithCancel(ctx)
	c := &Console{ctx: ctx, cancel: cancel, input: make(chan []byte, 32), prompts: make(chan consolePrompt, 1)}
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-c.input:
				if _, err := writer.Write(data); err != nil {
					return
				}
			}
		}
	}()
	go c.edit(reader, title, prompt, width, send)
	return c
}

func (c *Console) edit(in *io.PipeReader, title, prompt string, width int, send func(ConsoleEvent)) {
	defer c.Close()
	defer in.Close()
	stop := context.AfterFunc(c.ctx, func() { _ = in.Close() })
	defer stop()
	out := consoleWriter(func(p []byte) { send(ConsoleEvent{Output: p}) })
	var complete readline.AutoCompleter
	editor, err := readline.NewEx(&readline.Config{
		Prompt: prompt, Stdin: in, Stdout: out, Stderr: out,
		ForceUseInteractive: true, HistoryLimit: 100,
		FuncGetWidth: func() int { return max(20, width) },
		FuncMakeRaw:  func() error { return nil }, FuncExitRaw: func() error { return nil },
		FuncOnWidthChanged:  func(func()) {},
		FuncFilterInputRune: func(r rune) (rune, bool) { return r, r != readline.CharCtrlZ },
		AutoComplete:        consoleCompletion{current: &complete},
	})
	if err != nil {
		send(ConsoleEvent{Closed: true})
		return
	}
	defer editor.Close()
	for {
		select {
		case <-c.ctx.Done():
			return
		case screen := <-c.prompts:
			complete = screen.completer
			DrawConsole(out, title, screen.body)
		}
		line, err := editor.Readline()
		if err != nil {
			send(ConsoleEvent{Closed: true})
			return
		}
		send(ConsoleEvent{Line: &line})
	}
}

type consoleCompletion struct {
	current *readline.AutoCompleter
}

func (c consoleCompletion) Do(line []rune, pos int) ([][]rune, int) {
	if complete := *c.current; complete != nil {
		return complete.Do(line, pos)
	}
	return nil, 0
}

// Show displays the next prompt with completion for that prompt's context.
func (c *Console) Show(body string, completer readline.AutoCompleter) {
	select {
	case <-c.ctx.Done():
	case c.prompts <- consolePrompt{body: body, completer: completer}:
	}
}

// TryInput queues keyboard input without blocking the terminal owner.
func (c *Console) TryInput(data []byte) bool {
	select {
	case <-c.ctx.Done():
		return false
	case c.input <- data:
		return true
	default:
		return false
	}
}

func (c *Console) Close() { c.cancel() }
