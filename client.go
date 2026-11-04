package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xtaci/smux"
	"golang.org/x/term"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, s)
}
func localPath(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("path must be UTF-8")
	}
	s := strings.TrimSpace(string(b))
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		s = filepath.Join(home, s[2:])
	}
	return s, nil
}

type clientEvent struct {
	m        message
	input    []byte
	err      error
	task     *clientTransfer
	finished bool
	text     string
}
type clientTransfer struct {
	id        uint64
	stream    *smux.Stream
	request   message
	choice    chan string
	ctx       context.Context
	cancel    context.CancelFunc
	line      []byte
	started   bool
	progress  atomic.Int64
	total     atomic.Int64
	closeOnce sync.Once
}

func (t *clientTransfer) close() {
	t.cancel()
	// Close wakes blocked reads/writes immediately, but sending FIN may wait for
	// the transport. Do not perform that potentially slow operation in the UI.
	t.closeOnce.Do(func() { go func() { _ = t.stream.Close() }() })
}
func sshCommand(o options) *exec.Cmd {
	remote := shellQuote(o.server) + " bridge " + shellQuote(o.session)
	if o.list {
		remote = shellQuote(o.server) + " sessions"
	}
	exe := os.Getenv("SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	args := append([]string(nil), o.ssh...)
	args = append(args, "-T", "-o", "RequestTTY=no", "--", o.host, remote)
	return exec.Command(exe, args...)
}
func runClient(o options) (int, error) {
	cmd := sshCommand(o)
	cmd.Stderr = os.Stderr
	if o.list {
		cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
		err := cmd.Run()
		var e *exec.ExitError
		if errors.As(err, &e) {
			return e.ExitCode(), nil
		}
		return 0, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return 1, fmt.Errorf("an interactive terminal is required")
	}
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	writer, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	if err = cmd.Start(); err != nil {
		return 1, fmt.Errorf("start system OpenSSH: %w", err)
	}
	defer func() { _ = writer.Close(); _ = reader.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// The mux handshake happens after OpenSSH authentication; no local keyboard
	// reader or raw mode is active while OpenSSH prompts for passwords/host keys.
	transport := &pipeTransport{Reader: reader, Writer: writer, closers: []io.Closer{reader, writer}}
	mux, err := clientMux(transport)
	if err != nil {
		return 255, err
	}
	defer mux.Close()
	control, err := mux.OpenStream()
	if err != nil {
		return 255, err
	}
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	if err = streamMessage(control, message{Type: "attach", Version: protocolVersion, Cols: uint16(cols), Rows: uint16(rows), Term: termName}); err != nil {
		return 255, err
	}
	m, err := readMessage(control)
	if err != nil {
		return 255, err
	}
	if m.Type == "fatal" {
		_ = streamMessage(control, message{Type: "bye"})
		return 1, errors.New(m.Error)
	}
	if m.Type != "ready" || m.Version != protocolVersion {
		return 1, fmt.Errorf("unexpected attach handshake")
	}
	pid := m.PID
	_ = mux.SetDeadline(time.Now().Add(5 * time.Second))
	terminal, err := mux.AcceptStream()
	if err != nil {
		return 255, err
	}
	_ = mux.SetDeadline(time.Time{})
	_ = terminal.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err = readMessage(terminal)
	if err != nil {
		return 255, err
	}
	if m.Type != "terminal" {
		return 1, fmt.Errorf("missing terminal stream")
	}
	_ = terminal.SetReadDeadline(time.Time{})
	restore, err := rawTerminal()
	if err != nil {
		return 1, err
	}
	defer restore()
	fmt.Fprintf(os.Stderr, "[sshore] Connected %s / %s (PID %d)\r\n", cleanText(o.host), o.session, pid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	events := make(chan clientEvent, 32)
	emit := func(e clientEvent) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	}
	readEvents := func(st *smux.Stream) {
		for {
			m, err := readMessage(st)
			if err != nil {
				emit(clientEvent{err: err})
				return
			}
			emit(clientEvent{m: m})
			if m.Type == "exit" || m.Type == "detached" || m.Type == "fatal" {
				return
			}
		}
	}
	go readEvents(control)
	go readEvents(terminal)
	go func() {
		for {
			st, err := mux.AcceptStream()
			if err != nil {
				return
			}
			_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
			m, err := readMessage(st)
			_ = st.SetReadDeadline(time.Time{})
			if err != nil || m.ID == 0 || (m.Type != "upload" && m.Type != "offer") {
				_ = st.Close()
				continue
			}
			taskCtx, stop := context.WithCancel(ctx)
			t := &clientTransfer{id: m.ID, stream: st, request: m, choice: make(chan string, 1), ctx: taskCtx, cancel: stop}
			t.total.Store(m.Size)
			select {
			case events <- clientEvent{task: t}:
			case <-ctx.Done():
				t.close()
				return
			}
		}
	}()
	terminalInput := terminalInputReader()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := terminalInput.Read(buf)
			if n > 0 {
				emit(clientEvent{input: append([]byte(nil), buf[:n]...)})
			}
			if err != nil {
				emit(clientEvent{err: err})
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	// One terminal writer preserves keystroke order without making file I/O or a
	// stalled PTY block the main UI/control loop.
	inputQueue := make(chan []byte, 16)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-inputQueue:
				if err := streamMessage(terminal, message{Type: "input", Data: b}); err != nil {
					emit(clientEvent{err: err})
					return
				}
			}
		}
	}()
	var workers sync.WaitGroup
	tasks := make(map[uint64]*clientTransfer)
	var order []uint64
	var pending []*clientTransfer
	var prompt *clientTransfer
	var buffered []byte
	omitted := false
	defer func() {
		cancel()
		_ = mux.Close()
		for _, t := range tasks {
			t.close()
		}
		for _, t := range pending {
			t.close()
		}
		workers.Wait()
	}()
	print := func(format string, args ...any) { fmt.Fprintf(os.Stdout, format, args...) }
	showPrompt := func() {
		if prompt != nil {
			return
		}
		for _, id := range order {
			t := tasks[id]
			if t == nil || t.started {
				continue
			}
			prompt = t
			if t.request.Type == "upload" {
				print("\r\n[sshore #%d] Local file to upload (Ctrl-C cancels): ", t.id)
			} else {
				print("\r\n[sshore #%d] Download %s (%d bytes)\r\nSave as [./%s] (Ctrl-C cancels): ", t.id, t.request.Name, t.request.Size, t.request.Name)
			}
			return
		}
	}
	remove := func(t *clientTransfer) {
		delete(tasks, t.id)
		for i, id := range order {
			if id == t.id {
				order = append(order[:i], order[i+1:]...)
				break
			}
		}
		if prompt == t {
			prompt = nil
		}
	}
	cancelTask := func(t *clientTransfer) { t.close(); remove(t); print("\r\n[sshore #%d] Transfer cancelled\r\n", t.id) }
	startTask := func(t *clientTransfer) {
		if t.request.Type == "offer" {
			if e := safeName(t.request.Name); e != nil || t.request.Size < 0 {
				t.close()
				return
			}
		}
		tasks[t.id] = t
		order = append(order, t.id)
		if o.downloadDir != "" && t.request.Type == "offer" {
			t.started = true
			t.choice <- filepath.Join(o.downloadDir, t.request.Name)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			text, err := runFileTask(t)
			emit(clientEvent{task: t, finished: true, text: text, err: err})
		}()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	progressAt := time.Now()
	detaching := false
	var detachTimeout <-chan time.Time
	for {
		select {
		case <-signals:
			return 130, nil
		case <-detachTimeout:
			return 255, fmt.Errorf("detach response timed out; reconnect to the same session")
		case <-ticker.C:
			c, r, e := term.GetSize(int(os.Stdout.Fd()))
			if e == nil && (c != cols || r != rows) {
				cols, rows = c, r
				if err = streamMessage(control, message{Type: "resize", Cols: uint16(c), Rows: uint16(r)}); err != nil {
					return 255, err
				}
			}
			if !o.quiet && prompt == nil && time.Since(progressAt) >= time.Second {
				for _, id := range order {
					t := tasks[id]
					if t != nil && t.started {
						kind := "Download"
						if t.request.Type == "upload" {
							kind = "Upload"
						}
						print("\r\n[sshore #%d] %s: %d/%d bytes\r\n", id, kind, t.progress.Load(), t.total.Load())
					}
				}
				progressAt = time.Now()
			}
		case e := <-events:
			if e.task != nil {
				t := e.task
				if e.finished {
					if tasks[t.id] != t {
						continue
					}
					remove(t)
					if prompt != nil {
						print("\r\n")
					}
					if e.err != nil {
						print("\r\n[sshore #%d] %s\r\n", t.id, cleanText(e.err.Error()))
					} else {
						print("\r\n[sshore #%d] %s\r\n", t.id, cleanText(e.text))
					}
					if prompt != nil {
						print("[sshore #%d] Path: %s", prompt.id, string(prompt.line))
					}
				} else {
					if tasks[t.id] != nil {
						t.close()
						continue
					}
					if len(tasks) >= maxTransfers {
						if len(pending) >= maxTransfers {
							t.close()
						} else {
							pending = append(pending, t)
						}
					} else {
						startTask(t)
					}
				}
			} else if e.err != nil {
				return 255, fmt.Errorf("connection ended; reconnect to the same session: %w", e.err)
			} else if e.input != nil {
				if detaching {
					continue
				}
				if !o.noEscape && bytes.IndexByte(e.input, 0x1d) >= 0 {
					if err = streamMessage(control, message{Type: "detach"}); err != nil {
						return 255, err
					}
					detaching = true
					detachTimeout = time.After(5 * time.Second)
					continue
				}
				if bytes.IndexByte(e.input, 3) >= 0 && len(tasks) > 0 {
					t := prompt
					if t == nil {
						for i := len(order) - 1; i >= 0; i-- {
							if tasks[order[i]] != nil {
								t = tasks[order[i]]
								break
							}
						}
					}
					if t != nil {
						cancelTask(t)
					}
				} else if prompt != nil {
					submitted := false
					for _, b := range e.input {
						switch {
						case b == '\r' || b == '\n':
							submitted = true
						case b == 8 || b == 127:
							if len(prompt.line) > 0 {
								_, n := utf8.DecodeLastRune(prompt.line)
								prompt.line = prompt.line[:len(prompt.line)-n]
								print("\b \b")
							}
						case b >= 32 && b != 127 && len(prompt.line) < 4096:
							prompt.line = append(prompt.line, b)
							_, _ = os.Stdout.Write([]byte{b})
						}
						if submitted {
							break
						}
					}
					if submitted {
						print("\r\n")
						t := prompt
						prompt = nil
						path := t.request.Name
						var e error
						if len(t.line) > 0 || t.request.Type == "upload" {
							path, e = localPath(t.line)
						}
						if e != nil {
							t.close()
							remove(t)
							print("[sshore #%d] %s\r\n", t.id, cleanText(e.Error()))
						} else {
							t.started = true
							t.choice <- path
						}
					}
				} else {
					select {
					case inputQueue <- e.input:
					default:
						return 255, fmt.Errorf("terminal input stalled; reconnect to the same session")
					}
				}
			} else {
				switch e.m.Type {
				case "output":
					if prompt != nil {
						if len(buffered)+len(e.m.Data) <= 256*1024 {
							buffered = append(buffered, e.m.Data...)
						} else {
							omitted = true
						}
					} else {
						if err = writeFull(os.Stdout, e.m.Data); err != nil {
							return 1, err
						}
					}
				case "detach_requested":
					if !detaching {
						if err = streamMessage(control, message{Type: "detach"}); err != nil {
							return 255, err
						}
						detaching = true
						detachTimeout = time.After(5 * time.Second)
					}
				case "detached":
					_ = streamMessage(control, message{Type: "bye"})
					print("\r\n[sshore] Detached\r\n")
					return 0, nil
				case "exit":
					return e.m.Code, nil
				case "fatal":
					return 1, errors.New(e.m.Error)
				}
			}
		}
		if !detaching {
			for len(pending) > 0 && len(tasks) < maxTransfers {
				t := pending[0]
				pending = pending[1:]
				startTask(t)
			}
			showPrompt()
		}
		if prompt == nil && (len(buffered) > 0 || omitted) {
			if err = writeFull(os.Stdout, buffered); err != nil {
				return 1, err
			}
			buffered = nil
			if omitted {
				print("\r\n[sshore] Some output was omitted during the file prompt\r\n")
				omitted = false
			}
		}
	}
}
