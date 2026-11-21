package termui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
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
	prompt    string
	completer readline.AutoCompleter
}

type ConsoleEvent struct {
	Output []byte
	Line   *string
	Closed bool
}

type consoleWriter struct {
	mu      sync.Mutex
	pending []byte
	send    func([]byte)
}

func (w *consoleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.HasPrefix(p, []byte("\x1b[J\x1b[2K")) {
		w.pending = append(w.pending, p...)
	} else if len(w.pending) > 0 {
		// Readline emits the erase and redraw separately. Send them together
		// and let terminals that support synchronized output paint once.
		output := append([]byte("\x1b[?2026h"), w.pending...)
		output = append(output, p...)
		output = append(output, "\x1b[?2026l"...)
		w.pending = nil
		w.send(output)
	} else {
		w.send(bytes.Clone(p))
	}
	return len(p), nil
}

func (w *consoleWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) != 0 {
		w.send(w.pending)
		w.pending = nil
	}
}

func WriteConsoleBody(out io.Writer, body string) {
	writeConsoleBody(out, body, true)
}

func writeConsoleBody(out io.Writer, body string, color bool) {
	if body == "" {
		return
	}
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = cleanConsoleLine(lines[i], color)
	}
	fmt.Fprintf(out, "\r%s\r\n\r\n", strings.Join(lines, "\r\n"))
}

// cleanConsoleLine allows the fixed SGR sequences used by our renderers, while
// neutralizing other terminal controls in text received from a peer.
func cleanConsoleLine(line string, color bool) string {
	var out strings.Builder
	var bold, foreground bool
	for len(line) > 0 {
		i := strings.IndexByte(line, 0x1b)
		if i < 0 {
			out.WriteString(CleanText(line))
			break
		}
		out.WriteString(CleanText(line[:i]))
		line = line[i:]
		matched := false
		for _, code := range []string{ansi.ResetStyle, "\x1b[1m", "\x1b[1;36m", "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[34m", "\x1b[36m", "\x1b[39m", "\x1b[90m"} {
			if strings.HasPrefix(line, code) {
				if color {
					out.WriteString(code)
					switch code {
					case ansi.ResetStyle:
						bold, foreground = false, false
					case "\x1b[1m":
						bold = true
					case "\x1b[1;36m":
						bold, foreground = true, true
					case "\x1b[39m":
						foreground = false
					default:
						foreground = true
					}
				}
				line = line[len(code):]
				matched = true
				break
			}
		}
		if !matched {
			out.WriteRune('\ufffd')
			line = line[1:]
		}
	}
	if bold || foreground {
		out.WriteString(ansi.ResetStyle)
	}
	return out.String()
}

// StartConsole starts a Readline editor. The caller continues to own the
// terminal modes and serializes emitted output with its other terminal writes.
func StartConsole(ctx context.Context, prompt string, width int, color bool, send func(ConsoleEvent)) *Console {
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
	go c.edit(reader, prompt, width, color, send)
	return c
}

func (c *Console) edit(in *io.PipeReader, prompt string, width int, color bool, send func(ConsoleEvent)) {
	defer c.Close()
	defer in.Close()
	stop := context.AfterFunc(c.ctx, func() { _ = in.Close() })
	defer stop()
	out := &consoleWriter{send: func(p []byte) { send(ConsoleEvent{Output: p}) }}
	complete := &consoleCompletion{}
	editor, err := readline.NewEx(&readline.Config{
		Prompt: prompt, Stdin: in, Stdout: out, Stderr: out,
		ForceUseInteractive: true, HistoryLimit: 100,
		FuncGetWidth: func() int { return max(2, width) },
		FuncMakeRaw:  func() error { return nil }, FuncExitRaw: func() error { return nil },
		FuncOnWidthChanged:  func(func()) {},
		FuncFilterInputRune: complete.filter,
		Listener:            readline.FuncListener(complete.changed),
		AutoComplete:        complete,
	})
	if err != nil {
		out.flush()
		send(ConsoleEvent{Closed: true})
		return
	}
	defer func() { editor.Close(); out.flush() }()
	for {
		select {
		case <-c.ctx.Done():
			return
		case screen := <-c.prompts:
			complete.set(screen.completer)
			if screen.prompt != "" {
				editor.SetPrompt(screen.prompt)
				// The local panel owns this screen. Clear it before replacing the prompt.
				fmt.Fprint(out, "\x1b[2J\x1b[H")
			} else {
				editor.SetPrompt(prompt)
			}
			writeConsoleBody(out, screen.body, color)
		}
		line, err := editor.Readline()
		out.flush()
		if err != nil {
			send(ConsoleEvent{Closed: true})
			return
		}
		send(ConsoleEvent{Line: &line})
	}
}

type consoleCompletion struct {
	mu      sync.Mutex
	current readline.AutoCompleter
	cycle   pathCompletion
}

func (c *consoleCompletion) set(complete readline.AutoCompleter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current, c.cycle = complete, pathCompletion{}
}

func (c *consoleCompletion) filter(r rune) (rune, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r != readline.CharTab {
		c.cycle = pathCompletion{}
	}
	return r, r != readline.CharCtrlZ
}

func (c *consoleCompletion) changed(_ []rune, _ int, _ rune) ([]rune, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cycle.pending == nil {
		return nil, 0, false
	}
	line, pos := c.cycle.pending, c.cycle.pendingPos
	c.cycle.pending = nil
	return line, pos, true
}

// Readline's candidate grid assumes terminal newline processing and can scroll
// the full-screen prompt. Cycle matches in the input line instead.
func (c *consoleCompletion) Do(line []rune, pos int) ([][]rune, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.cycle
	if len(state.candidates) == 0 {
		if c.current == nil {
			return nil, 0
		}
		candidates, offset := c.current.Do(line, pos)
		if len(candidates) <= 1 {
			return candidates, offset
		}
		if common, n := (readline.Runes{}).Aggregate(candidates); n > 0 {
			return [][]rune{common}, offset
		}
		state.line, state.pos = slices.Clone(line), pos
		state.candidates, state.selected = candidates, -1
	}
	state.cycle()
	return nil, 0
}

// Show displays the next prompt with completion for that prompt's context.
func (c *Console) Show(body string, completer readline.AutoCompleter) {
	c.show(consolePrompt{body: body, completer: completer})
}

// Replace clears the panel and presents a different prompt without completion.
func (c *Console) Replace(body, prompt string) {
	c.show(consolePrompt{body: body, prompt: prompt})
}

func (c *Console) show(screen consolePrompt) {
	select {
	case <-c.ctx.Done():
	case c.prompts <- screen:
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
