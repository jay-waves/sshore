package main

// The client launches system OpenSSH and adapts its pipes to the multiplexed
// transport. Remote requests only save approval metadata and detach; separately
// approved file operations finish before terminal interaction resumes. During
// approval execution and --stay, normal remote output is discarded, not replayed.
// Each connection disables ControlMaster/ControlPath/ControlPersist sharing so it
// owns its transport lifetime. Other local SSH configuration remains effective.
// SSH agent/X11 environments belong to the connection that created the remote
// shell and are not refreshed on reattachment. Noninteractive login-script stdout
// can corrupt the protocol handshake; diagnostics belong on stderr.

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
			return '\ufffd'
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
	if s == "~" || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if s == "~" {
			s = home
		} else {
			s = filepath.Join(home, s[2:])
		}
	}
	return s, nil
}

type clientEvent struct {
	approvalStream *smux.Stream
	approval       []operation
	m              message
	input          []byte
	err            error
}

func sshCommand(o options) *exec.Cmd {
	remote := remoteCommand(o)

	exe := os.Getenv("SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	// Own this transport: never reuse or leave behind a shared OpenSSH master.
	args := []string{"-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no"}
	args = append(args, o.ssh...)
	args = append(args, "-T", "-o", "RequestTTY=no", "--", o.host, remote)
	return exec.Command(exe, args...)
}

// Recovery has no persistent session name. Only automatic managed connections
// learn their name from the daemon, before saving any approval metadata.
func acceptSessionName(o *options, name string) error {
	if o.plain || o.session != "" {
		return nil
	}
	if err := validSession(name); err != nil {
		return fmt.Errorf("server returned an invalid session name: %w", err)
	}
	o.session = name
	return nil
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
	resetTerminalInputModes()
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
	if err = negotiateSetup(reader, writer, &o); err != nil {
		return 1, err
	}
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
	action := ""
	if len(o.approved) > 0 {
		action = "approved"
	}
	if err = streamMessage(control, message{Type: "attach", Action: action, PID: o.resumePID, Cols: uint16(cols), Rows: uint16(rows), Term: termName}); err != nil {
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
	if m.Type != "ready" {
		return 1, fmt.Errorf("unexpected attach handshake")
	}
	if err := acceptSessionName(&o, m.Name); err != nil {
		return 1, err
	}
	if len(o.approved) > 0 && m.PID != o.resumePID {
		return 1, fmt.Errorf("remote session changed; request again")
	}
	pid := m.PID
	o.resumePID = pid
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
	if len(o.approved) > 0 {
		opCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = executeApprovedFiles(opCtx, mux, o.approved, o.quiet)
		stop()
		if err != nil {
			return 1, err
		}
		if o.stay {
			forwards := newForwardManager(context.Background(), mux)
			defer forwards.close()
			for _, op := range o.approved {
				if op.Kind == "forward" {
					r := forwards.request(message{Action: "add", Port: op.Port})
					if r.Error != "" {
						return 1, fmt.Errorf("%s", r.Error)
					}
					return stayWithForwards(mux, control, terminal)
				}
			}
			// --stay explicitly detaches after completed file operations.
			if err = detachControl(control); err != nil {
				return 1, err
			}
			logf(os.Stderr, logSuccess, "[sshore] Approved operations complete. Staying local; remote shell is still running.\n")
			return 0, nil
		}
	}
	if len(o.approved) > 0 {
		if err = streamMessage(control, message{Type: "resume_output"}); err != nil {
			return 1, err
		}
		_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
		ack, e := readMessage(control)
		_ = control.SetReadDeadline(time.Time{})
		if e != nil {
			return 1, e
		}
		if ack.Type != "output_resumed" {
			return 1, fmt.Errorf("unexpected resume response")
		}
	}
	restore, err := rawTerminal()
	if err != nil {
		return 1, err
	}
	defer func() { restore(); _ = flushTerminalInput() }()
	if o.plain {
		logf(os.Stderr, logWarning, "[sshore] Recovery shell on %s (no sshore features).\r\nExit and reconnect to resume normal use.\r\n", cleanText(o.host))
	} else {

		logf(os.Stderr, logSuccess, "[sshore] Connected %s / %s (PID %d)\r\n", cleanText(o.host), o.session, pid)
		logf(os.Stderr, logInfo, "[sshore] Reconnect with --session %s\r\n", o.session)
	}
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
	forwards := newForwardManager(ctx, mux)
	defer forwards.close()
	for _, op := range o.approved {
		if op.Kind == "forward" {
			r := forwards.request(message{Action: "add", Port: op.Port})
			if r.Error != "" {
				return 1, fmt.Errorf("%s", r.Error)
			}
			logf(os.Stderr, logSuccess, "[sshore] Forwarding %s -> remote %s\r\n", loopback(op.Port), loopback(op.Port))
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
			if err == nil && m.Type == "approval_request" {
				emit(clientEvent{approvalStream: st, approval: m.Operations})
				continue
			}
			if err == nil && m.Type == "forward" && (m.Action == "list" || m.Action == "stop") {
				forwards.handle(st, m)
				continue
			}
			// Never let unsolicited offers, uploads, or forward-add requests
			// enter a file worker or open a listener, even from a malicious peer.
			_ = st.Close()
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
	color := terminalLogColor(os.Stdout)
	notice := func(level logColor, format string, args ...any) {
		fmt.Fprint(os.Stdout, formatLog(color, level, format, args...))
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var outputMouse terminalMouseState
	detaching := false
	var detachTimeout <-chan time.Time
	for {
		select {
		case <-signals:
			return 130, nil
		case <-detachTimeout:
			return 255, fmt.Errorf("detach response timed out; session preservation is unconfirmed")
		case <-ticker.C:
			c, r, e := term.GetSize(int(os.Stdout.Fd()))
			if e == nil && (c != cols || r != rows) {
				cols, rows = c, r
				if err = streamMessage(control, message{Type: "resize", Cols: uint16(c), Rows: uint16(r)}); err != nil {
					return 255, err
				}
			}

		case e := <-events:
			if e.approvalStream != nil {
				if o.plain {
					e.approvalStream.Close()
					continue
				}
				st := e.approvalStream
				if detaching {
					st.Close()
					continue
				}
				id, err := writeApproval(o, e.approval)
				if err != nil {
					_ = streamMessage(st, message{Type: "error", Error: err.Error()})
					st.Close()
					notice(logError, "\r\n[sshore] Request rejected: %s\r\n", cleanText(err.Error()))
					continue
				}
				_ = st.SetDeadline(time.Now().Add(forwardTimeout))
				err = writeMessage(st, message{Type: "queued"})
				if err == nil {
					var ack message
					ack, err = readMessage(st)
					if err == nil && ack.Type != "queued_ack" {
						err = fmt.Errorf("unexpected request acknowledgement")
					}
				}
				st.Close()
				if err != nil {
					dir, e := approvalDir()
					if e == nil {
						_ = os.Remove(filepath.Join(dir, id+".json"))
					}
					return 1, err
				}
				notice(logInfo, "\r\n[sshore] Request saved: %s\r\n[sshore] Run sshore approve from your LOCAL shell after this client exits.\r\n", id)
				if err = streamMessage(control, message{Type: "detach"}); err != nil {
					return 1, err
				}
				detaching = true
				detachTimeout = time.After(5 * time.Second)
				continue
			}
			if e.err != nil {
				return 255, fmt.Errorf("connection ended without detach; remote session is closing: %w", e.err)
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
				select {
				case inputQueue <- e.input:
				default:
					return 255, fmt.Errorf("terminal input stalled; closing remote session")
				}
			} else {
				switch e.m.Type {
				case "output":
					if detaching {
						continue
					}
					if err = writeFull(os.Stdout, e.m.Data); err != nil {
						return 1, err
					}
					outputMouse.observe(e.m.Data)
					if err = syncTerminalMouse(&outputMouse); err != nil {
						return 1, fmt.Errorf("update terminal mouse mode: %w", err)
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
					notice(logSuccess, "\r\n[sshore] Detached\r\n")
					return 0, nil
				case "exit":
					return e.m.Code, nil
				case "fatal":
					return 1, errors.New(e.m.Error)
				}
			}
		}
	}
}

// detachControl is used before an interactive control reader has started.
func detachControl(control *smux.Stream) error {
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	defer control.SetDeadline(time.Time{})
	if err := streamMessage(control, message{Type: "detach"}); err != nil {
		return err
	}
	m, err := readMessage(control)
	if err != nil {
		return err
	}
	if m.Type != "detached" {
		return fmt.Errorf("unexpected detach response: %s", m.Type)
	}
	return streamMessage(control, message{Type: "bye"})
}
