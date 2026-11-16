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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xtaci/smux"
	"golang.org/x/term"

	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
	transportio "github.com/jay-waves/sshore/internal/transport"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

type clientEvent struct {
	keyAction      tmux.KeyAction
	keyDone        chan struct{}
	inputFailure   bool
	approvalStream *smux.Stream
	approval       []operation
	m              message
	input          []byte
	err            error
}

func sshCommand(o options) *exec.Cmd {
	remote := remoteCommand(o)

	exe := sshExecutable()
	// Own this transport: never reuse or leave behind a shared OpenSSH master.
	args := []string{"-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no"}
	args = append(args, o.ssh...)
	args = append(args, "-T", "-o", "RequestTTY=no", "--", o.host, remote)
	cmd := exec.Command(exe, args...)
	cmd.Dir = o.sshDirectory
	return cmd
}

// Recovery has no persistent session name. Only automatic managed connections
// learn their name from the daemon, before saving any approval metadata.
func acceptSessionName(o *options, name string) error {
	if o.plain || o.session != "" {
		return nil
	}
	if err := validSession(name); err != nil {
		return fmt.Errorf("server returned an invalid shell name: %w", err)
	}
	o.session = name
	return nil
}

func runClientOnce(o options, previous string, ready func()) (int, error) {
	for _, op := range o.approved {
		if op.LocalPortPending() {
			return 2, usage("choose a local port in review before execution")
		}
	}
	cancelledInstance := o.instance
	if o.command == "cancel" {
		if cleared, err := forgetEndedShell(o.host, o.session, o.instance); cleared || err != nil {
			return 0, err
		}
		snapshots, err := readShellSnapshots()
		if err != nil {
			return 1, err
		}
		for _, s := range snapshots {
			if s.Host == o.host && s.Name == o.session && (o.instance == "" || s.Instance == o.instance) {
				cancelledInstance = s.Instance
				break
			}
		}
	}
	cmd := sshCommand(o)
	cmd.Stderr = os.Stderr
	if o.list || o.command == "cancel" || o.command == "cancel-resource" || o.command == "rename" {
		cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
		err := cmd.Run()
		if err == nil && o.command == "cancel" {
			if cacheErr := confirmCancelledShell(o.host, o.session, cancelledInstance); cacheErr != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Cancelled; local history not updated: %s\n", cleanText(cacheErr.Error()))
			}
		}
		if err == nil && o.command == "rename" {
			if cacheErr := rememberShellRename(o.host, o.session, o.newName); cacheErr != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Renamed; local history not updated: %s\n", cleanText(cacheErr.Error()))
			}
		}
		var e *exec.ExitError
		if errors.As(err, &e) {
			return e.ExitCode(), nil
		}
		return 0, err
	}
	if !o.detached && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
		return 1, fmt.Errorf("an interactive terminal is required")
	}
	tmux.ResetInputModes()
	transport, closeTransport, err := openClientTransport(&o)
	if err != nil {
		return 1, err
	}
	defer closeTransport()
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
	if o.command == "attach" {
		action = "existing"
	}
	if len(o.approved) > 0 {
		action = "approved"
	}
	if err = streamMessage(control, message{Type: "attach", Tree: currentShellTree(), Instance: o.instance, Action: action, PID: o.resumePID, Cols: uint16(cols), Rows: uint16(rows), Term: termName, NoProfile: o.noProfile && (o.command == "launch" || o.command == "spawn") && !o.plain, NoLogin: o.noLogin && (o.command == "launch" || o.command == "spawn") && !o.plain, ApprovalID: o.approvalID}); err != nil {
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
		return 1, fmt.Errorf("remote shell changed; request again")
	}
	if len(m.Tree) > 0 {
		if warn, err := observeIdentityRoute(m.Tree[len(m.Tree)-1].MachineID, o.host, o.ssh); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Identity tracking failed: %s\n", cleanText(err.Error()))
		} else if warn {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Same identity through a new entry point; if this is a clone, run sshore reset there.\n")
		}
	}
	rememberShellTree(m.Tree)
	o.instance = m.Instance
	o.temporary = m.Temporary
	connection, cacheErr := trackShellConnection(o)
	if cacheErr != nil {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Shell tracking failed: %s\n", cleanText(cacheErr.Error()))
	}
	defer connection.close()
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

	if o.detached {
		if err := detachControl(control, func(name string) { connection.confirm(name, "detached") }); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stdout, "Started %s (%s).\n", shellTaskName(cleanText(o.session), o.temporary), cleanText(o.host))
		return 0, nil
	}
	if len(o.approved) > 0 {
		opCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = executeApprovedFiles(opCtx, mux, o.approved, o.quiet)
		stop()
		if err != nil {
			return 1, err
		}
		if o.stay {
			// --stay explicitly detaches after completed file operations.
			if err = detachControl(control, func(name string) { connection.confirm(name, "detached") }); err != nil {
				return 1, err
			}
			termui.Log(os.Stderr, termui.Success, "[sshore] Done. Staying local; remote shell running.\n")
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
	if !o.local && !o.plain && !o.quiet {
		if err := showConnectedTasks(control, o); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Task list unavailable: %s\n", cleanText(err.Error()))
		}
	}
	restore, err := tmux.RawConsole()
	if err != nil {
		return 1, err
	}
	defer func() { restore(); _ = tmux.FlushInput() }()
	terminalOutput, flushOutput := tmux.OutputWriter()
	defer func() { _ = flushOutput() }()
	if !o.plain && !o.local {
		if e := rememberConnection(o); e != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Connection history not saved: %s\n", cleanText(e.Error()))
		}
	}
	if o.plain {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Recovery on %s; sshore disabled. Exit and reconnect to restore.\r\n", cleanText(o.host))
	} else {
		kind := "launch"
		if o.temporary {
			kind = "spawn"
		}
		sessionNotice(termui.Success, "Connected %s / %s (%s)", cleanText(o.host), cleanText(o.session), kind)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	events := make(chan clientEvent, 32)
	inputDrained := make(chan struct{}, 1)
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
			if st == terminal && m.Type == "input_drained" {
				select {
				case inputDrained <- struct{}{}:
				case <-ctx.Done():
					return
				}
				continue
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
			if err == nil && m.Type == "approval_request" {
				emit(clientEvent{approvalStream: st, approval: m.Operations})
				continue
			}
			// Never let unsolicited offers, uploads, or forward-add requests
			// enter a file worker or open a listener, even from a malicious peer.
			_ = st.Close()
		}
	}()
	stopKeyboard, err := tmux.StartKeyboard(ctx, func(data []byte, err error) {
		if len(data) > 0 {
			emit(clientEvent{input: data})
		}
		if err != nil {
			emit(clientEvent{err: err})
		}
	})
	if err != nil {
		return 1, err
	}
	defer func() { cancel(); stopKeyboard() }()
	ready()
	// One terminal writer preserves keystroke order without making file I/O or a
	// stalled PTY block the main UI/control loop.
	inputQueue := make(chan []tmux.KeyEvent, 16)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case batch := <-inputQueue:
				for _, b := range batch {
					if b.Action != tmux.KeyNone {
						if b.Action == tmux.KeyDetach {
							if err := streamMessage(terminal, message{Type: "input_barrier"}); err != nil {
								emit(clientEvent{err: err, inputFailure: true})
								return
							}
							select {
							case <-inputDrained:
							case <-ctx.Done():
								return
							case <-time.After(5 * time.Second):
								emit(clientEvent{err: fmt.Errorf("terminal input acknowledgement timed out"), inputFailure: true})
								return
							}
						}
						done := make(chan struct{})
						emit(clientEvent{keyAction: b.Action, keyDone: done})
						select {
						case <-done:
						case <-ctx.Done():
							return
						}
						if b.Action == tmux.KeyDetach {
							return
						}
						continue
					}
					if err := streamMessage(terminal, message{Type: "input", Data: b.Data}); err != nil {
						emit(clientEvent{err: err, inputFailure: true})
						return
					}
				}
			}
		}
	}()
	color := termui.ColorEnabled(os.Stdout)
	notice := func(level termui.Color, format string, args ...any) {
		fmt.Fprint(os.Stdout, termui.Format(color, level, format, args...))
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var outputMouse tmux.InputState
	keys := tmux.Keys{Bypass: o.noEscape || o.plain}
	detaching := false
	detachPending := false
	var detachTimeout <-chan time.Time
	var inputFlushTimeout <-chan time.Time
	var switchTimeout <-chan time.Time
	var switchPending bool
	var switchAction string
	var switchTarget *options
	var chooser *tmux.SessionChooser
	queueSwitch := func(name, command string) error {
		if name == o.session && command == "attach" {
			return nil
		}
		target := o
		target.command = command
		target.temporary = command == "spawn"
		if command != "spawn" {
			target.owner = spawnOwner{}
		}
		target.taskIdentity = ""
		target.instance = ""
		target.session = name
		target.resumePID = 0
		target.approved = nil
		target.approvalID = ""
		target.stay = false
		select {
		case inputQueue <- []tmux.KeyEvent{{Action: tmux.KeyDetach}}:
			switchTarget = &target
			switchPending = false
			chooser = nil
			detachPending = true
			inputFlushTimeout = time.After(5 * time.Second)
			return nil
		default:
			return fmt.Errorf("terminal input queue is busy")
		}
	}
	requestTargets := func(action, name string) error {
		switchPending = true
		switchAction = action
		switchTimeout = time.After(5 * time.Second)
		return streamMessage(control, message{Type: "session_targets", Action: action, Name: name})
	}
	for {
		select {
		case <-signals:
			return 130, nil
		case <-switchTimeout:
			switchTimeout = nil
			switchPending = false
			chooser = nil
			notice(termui.Warning, "\r\n[sshore] Session lookup timed out; staying here.\r\n")
		case <-inputFlushTimeout:
			inputFlushTimeout = nil
			if !detaching {
				notice(termui.Warning, "\r\n[sshore] Input delivery unconfirmed; detaching.\r\n")
				if err = streamMessage(control, message{Type: "detach"}); err != nil {
					return 255, err
				}
				detaching = true
				detachTimeout = time.After(5 * time.Second)
			}
		case <-detachTimeout:
			return 255, fmt.Errorf("detach response timed out; shell preservation is unconfirmed")
		case <-ticker.C:
			c, r, e := term.GetSize(int(os.Stdout.Fd()))
			if e == nil && (c != cols || r != rows) {
				cols, rows = c, r
				if err = streamMessage(control, message{Type: "resize", Cols: uint16(c), Rows: uint16(r)}); err != nil {
					return 255, err
				}
			}

		case e := <-events:
			if e.keyAction != tmux.KeyNone {
				err = nil
				func() {
					defer close(e.keyDone)
					if detaching {
						return
					}
					switch e.keyAction {
					case tmux.KeyNew, tmux.KeyNext, tmux.KeyPrevious, tmux.KeyLast, tmux.KeyChoose:
						action := map[tmux.KeyAction]string{tmux.KeyNew: "new", tmux.KeyNext: "next", tmux.KeyPrevious: "previous", tmux.KeyLast: "last", tmux.KeyChoose: "choose"}[e.keyAction]
						err = requestTargets(action, previous)

					case tmux.KeyDetach:
						err = streamMessage(control, message{Type: "detach"})
						if err == nil {
							detaching = true
							detachTimeout = time.After(5 * time.Second)
						}
					case tmux.KeyRedraw:
						err = streamMessage(control, message{Type: "redraw"})
					case tmux.KeyInfo:
						notice(termui.Info, "\r\n[sshore] %s / %s (PID %d)\r\n", cleanText(o.host), cleanText(o.session), pid)
					case tmux.KeyHelp:
						notice(termui.Info, "\r\nCtrl+B: c new · n/p next/prev · l last · s/w sessions\r\n        d detach · r redraw · i info · ? help · Ctrl+B send prefix\r\n")
					}
				}()
				if err != nil {
					return 255, err
				}
				continue
			}
			if e.approvalStream != nil {
				if o.plain {
					e.approvalStream.Close()
					continue
				}
				st := e.approvalStream
				if detaching || detachPending || switchPending {
					st.Close()
					continue
				}
				id, err := writeApproval(o, e.approval)
				if err != nil {
					_ = streamMessage(st, message{Type: "error", Error: err.Error()})
					st.Close()
					notice(termui.Error, "\r\n[sshore] Request rejected: %s\r\n", cleanText(err.Error()))
					continue
				}
				_ = st.SetDeadline(time.Now().Add(requestTimeout))
				dir, err := approvalDir()
				if err != nil {
					st.Close()
					return 1, err
				}
				request, err := readApproval(dir, id)
				if err != nil {
					st.Close()
					return 1, err
				}
				task := pendingRemoteTask(id, request.DisplayName(), e.approval)
				err = writeMessage(st, message{Type: "queued", PeerTask: &task})
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
				notice(termui.Info, "\r\n[sshore] Request saved: %s\r\n[sshore] Run sshore review from your LOCAL shell after this client exits.\r\n", id)
				if err = streamMessage(control, message{Type: "detach"}); err != nil {
					return 1, err
				}
				detaching = true
				detachTimeout = time.After(5 * time.Second)
				continue
			}
			if e.err != nil {
				if e.inputFailure && detachPending {
					if !detaching {
						notice(termui.Warning, "\r\n[sshore] Input delivery unconfirmed; detaching.\r\n")
						if err = streamMessage(control, message{Type: "detach"}); err != nil {
							return 255, err
						}
						detaching = true
						detachTimeout = time.After(5 * time.Second)
					}
					inputFlushTimeout = nil
					continue
				}
				return 255, fmt.Errorf("connection lost; reconnect to check the shell: %w", e.err)
			} else if e.input != nil {
				if chooser != nil {
					selected, done := chooser.Feed(e.input)
					if done {
						chooser = nil
						switchPending = false
						if selected != "" && selected != o.session {
							if err = requestTargets("select", selected); err != nil {
								return 255, err
							}
						} else {
							notice(termui.Info, "\r\n")
							_ = streamMessage(control, message{Type: "redraw"})
						}
					} else {
						notice(termui.Info, "\rSession # (Esc cancels): %s \x1b[K", chooser.Number)
					}
					continue
				}
				if switchPending {
					continue
				}
				if detaching || detachPending {
					continue
				}
				batch := keys.Feed(e.input)
				if len(batch) > 0 {
					select {
					case inputQueue <- batch:
						for _, key := range batch {
							if key.Action.IsSessionCommand() {
								switchPending = true
							}
							if key.Action == tmux.KeyDetach {
								detachPending = true
								inputFlushTimeout = time.After(5 * time.Second)
								break
							}
						}
					default:
						return 255, fmt.Errorf("terminal input stalled; closing connection; reconnect to check the shell")
					}
				}

			} else {
				switch e.m.Type {
				case "session_targets":
					if !switchPending || e.m.Action != switchAction {
						continue
					}
					switchTimeout = nil
					switchPending = false
					if e.m.Error != "" {
						notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
						continue
					}
					if len(e.m.Names) > 128 || validSession(e.m.Name) != nil {
						return 255, fmt.Errorf("invalid session targets")
					}
					for _, name := range e.m.Names {
						if validSession(name) != nil {
							return 255, fmt.Errorf("invalid session target")
						}
					}
					o.session = e.m.Name
					var target string
					switch e.m.Action {
					case "new", "select":
						if len(e.m.Names) != 1 {
							return 255, fmt.Errorf("missing session target")
						}
						target = e.m.Names[0]
					case "next":
						target = tmux.AdjacentSession(o.session, e.m.Names, 1)
					case "previous":
						target = tmux.AdjacentSession(o.session, e.m.Names, -1)
					case "last":
						for _, name := range e.m.Names {
							if name == previous {
								target = name
								break
							}
						}
						if target == "" {
							notice(termui.Info, "\r\n[sshore] Previous session unavailable.\r\n")
							continue
						}
					case "choose":
						chooser = &tmux.SessionChooser{Names: tmux.SessionOrder(o.session, e.m.Names)}
						switchPending = true
						notice(termui.Info, "\r\n")
						for i, name := range chooser.Names {
							mark := ""
							if name == o.session {
								mark = " *"
							}
							notice(termui.Info, "%d  %s%s\r\n", i+1, name, mark)
						}
						notice(termui.Info, "Session # (Esc cancels): ")
						continue
					}
					command := "attach"
					if e.m.Action == "new" {
						command = "launch"
						if o.temporary {
							command = "spawn"
							if o.owner.PID == 0 {
								o.owner, err = currentSpawnOwner()
								if err != nil {
									return 1, err
								}
							}
						}
					}
					if err = queueSwitch(target, command); err != nil {
						notice(termui.Warning, "\r\n[sshore] %s\r\n", err.Error())
					}

				case "output":
					if detaching {
						continue
					}
					if err = transportio.WriteFull(terminalOutput, e.m.Data); err != nil {
						return 1, err
					}
					outputMouse.Observe(e.m.Data)
					if err = tmux.SyncMouse(&outputMouse); err != nil {
						return 1, fmt.Errorf("update terminal mouse mode: %w", err)
					}
				case "detach_requested":
					if !detaching && !detachPending {
						if err = streamMessage(control, message{Type: "detach"}); err != nil {
							return 255, err
						}
						detaching = true
						detachTimeout = time.After(5 * time.Second)
					}
				case "detached":
					connection.confirm(e.m.Name, "detached")
					if validSession(e.m.Name) == nil {
						o.session = e.m.Name
					}
					_ = streamMessage(control, message{Type: "bye"})
					if switchTarget != nil {
						return 0, &sessionSwitch{from: o, to: *switchTarget}
					}
					if o.temporary {
						sessionNotice(termui.Success, "Background; %s on %s ends with the original parent shell.\r\nReturn: %s", shellTaskName(cleanText(o.session), true), cleanText(o.host), reconnectCommand(o))
					} else {
						sessionNotice(termui.Success, "Detached; %s on %s keeps running.\r\nReconnect: %s", shellTaskName(cleanText(o.session), false), cleanText(o.host), reconnectCommand(o))
					}
					return 0, nil
				case "exit":
					connection.confirm(e.m.Name, "exited")
					if !o.plain {
						level := termui.Success
						if e.m.Code != 0 {
							level = termui.Warning
						}
						sessionNotice(level, "Exited %s on %s (code %d).", shellTaskName(cleanText(o.session), o.temporary), cleanText(o.host), e.m.Code)
					}
					return e.m.Code, nil
				case "fatal":
					return 1, errors.New(e.m.Error)
				}
			}
		}
	}
}

func sessionNotice(level termui.Color, format string, args ...any) {
	termui.Log(os.Stderr, level, "\r\n\r\n\r\n[sshore] "+format+"\r\n\r\n", args...)
}

// detachControl is used before an interactive control reader has started.
func detachControl(control *smux.Stream, confirmed ...func(string)) error {
	return tmux.Detach(control, confirmed...)
}

// Retain transport options in the reconnect hint, including custom ports and keys.
// Use POSIX shell quoting on Linux and PowerShell quoting on Windows.
func reconnectCommand(o options) string {
	quote := func(s string) string {
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%_+=:,./-", r))
		}) < 0 {
			return s
		}
		if runtime.GOOS == "windows" {
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		}
		return shellQuote(s)
	}
	args := []string{"sshore", "attach", o.host, o.session}
	if o.local {
		args = []string{"sshore", "attach", "--local", o.session}
	}
	if o.server != "" && o.server != "sshore" {
		args = append(args, "--server", o.server)
	}
	if len(o.ssh) > 0 {
		args = append(args, "--")
		args = append(args, o.ssh...)
	}
	for i := range args {
		args[i] = quote(args[i])
	}
	return strings.Join(args, " ")
}
