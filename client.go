package main

// The frontend owns terminal interaction; daemon-owned tasks outlive its
// connections. Remote operation requests are approved in the local terminal.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/jay-waves/sshore/internal/pty"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// An interrupt cancels setup, but becomes terminal input after attachment.
// Recovery setup ignores repeated Ctrl+C so one cancellation cannot unwind
// multiple parent attachments. SIGTERM remains a cancellation in every phase.
type clientSignals struct {
	interactive atomic.Bool
	interrupts  chan struct{}
}

func newClientSignals(recovering bool) (context.Context, *clientSignals, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &clientSignals{interrupts: make(chan struct{}, 1)}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-signals:
				if sig == os.Interrupt && s.interactive.Load() {
					select {
					case s.interrupts <- struct{}{}:
					default:
					}
				} else if sig != os.Interrupt || !recovering {
					cancel()
				}
			}
		}
	}()
	return ctx, s, func() {
		signal.Stop(signals)
		cancel()
		<-done
	}
}

type clientEvent struct {
	consoleSource *commandConsole
	console       termui.ConsoleEvent
	inputBarrier  bool
	inputOffset   uint64
	keyAction     pty.KeyAction
	inputFailure  bool
	m             *message
	input         []byte
	output        []byte
	err           error
}

// Put all terminal bytes on the event queue before publishing a confirmed exit.
func readAttachmentEvents(ctx context.Context, control attachmentControl, terminal io.Reader, emit func(clientEvent), inputDrained chan<- struct{}) {
	var outputBytes atomic.Uint64
	outputProgress := make(chan struct{}, 1)
	outputDone := make(chan struct{})
	go func() {
		for {
			m, err := readMessage(control)
			if err != nil {
				emit(clientEvent{err: err})
				return
			}
			if m.Type == "exit" && outputBytes.Load() < m.TerminalOffset {
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for outputBytes.Load() < m.TerminalOffset {
					select {
					case <-outputProgress:
						deadline.Reset(5 * time.Second)
					case <-deadline.C:
						if outputBytes.Load() < m.TerminalOffset {
							emit(clientEvent{err: io.ErrUnexpectedEOF})
							return
						}
					case <-outputDone:
						if outputBytes.Load() < m.TerminalOffset {
							emit(clientEvent{err: io.ErrUnexpectedEOF})
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}
			if m.Type == "input_drained" {
				select {
				case inputDrained <- struct{}{}:
				case <-ctx.Done():
					return
				}
				continue
			}
			emit(clientEvent{m: &m})
			if m.Type == "exit" || m.Type == "detached" || m.Type == "fatal" {
				return
			}
		}
	}()
	go func() {
		defer close(outputDone)
		buf := make([]byte, 32*1024)
		for {
			n, e := terminal.Read(buf)
			if n > 0 {
				emit(clientEvent{output: append([]byte(nil), buf[:n]...)})
				outputBytes.Add(uint64(n))
				select {
				case outputProgress <- struct{}{}:
				default:
				}
			}
			if e != nil {
				// The control stream can still have a confirmed exit in
				// flight when a relay closes the terminal connection.
				_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
				return
			}
		}
	}()
}

// Learn the current shell name before saving approval metadata.
func acceptSessionName(o *options, name string) error {
	if o.session != "" && !(o.command == "attach" && (!o.local || o.hosted)) {
		return nil
	}
	if err := validSession(name); err != nil {
		return fmt.Errorf("server returned an invalid shell name: %w", err)
	}
	o.session = name
	return nil
}

func runClientOnce(o options, previous string, rootFrontend bool, transports clientTransports, ready func(options) string) (resultCode int, resultErr error) {
	attached := false
	var ended func()
	// Report after input shutdown and console restoration, including nested returns.
	defer func() {
		if ended != nil {
			ended()
			return
		}
		_, switching := errors.AsType[*sessionSwitch](resultErr)
		_, approving := errors.AsType[*localApproval](resultErr)
		if attached && resultErr != nil && !switching && !approving {
			sessionNotice(termui.Warning, fmt.Sprintf("Disconnected from %s (%s). Shell state unconfirmed.", cleanText(shellDisplayEndpoint(o)), sessionDisconnectReason(resultErr)), "")
		}
	}()
	signalCtx, interruptSignals, stopSignals := newClientSignals(o.recovering)
	defer stopSignals()
	// Cancel I/O before restoring the console, but keep signal handling installed
	// until the keyboard has stopped and terminal restoration has completed.
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	if o.command == "kill" || o.command == "rename" {
		target, lookupErr := participatingMutationTarget(o)
		if lookupErr != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Could not read participating shells: %s\n", cleanText(lookupErr.Error()))
		}
		err := transportio.Run(context.Background(), sshConnectionOptions(o), os.Stdin, os.Stdout, os.Stderr)
		if err == nil && target.Instance != "" {
			var updateErr error
			if o.command == "kill" {
				if validTaskUID(o.instance) {
					updateErr = rememberShellExit(o)
				} else {
					updateErr = forgetParticipatingShell(o.host, target.Instance)
				}
			} else {
				updateErr = renameParticipatingShell(o.host, target.Instance, o.newName)
			}
			if updateErr != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Could not update participating shells: %s\n", cleanText(updateErr.Error()))
			}
		}
		if e, ok := errors.AsType[exitStatusError](err); ok {
			return e.ExitStatus(), nil
		}
		return 0, err
	}
	if !o.detached && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
		return 1, fmt.Errorf("an interactive terminal is required")
	}
	pty.ResetInputModes()
	transport, err := openClientTransportContext(ctx, &o, transports)
	if err != nil {
		return 1, err
	}
	defer transport.Close()
	control := transport
	if err := clientAttachment(control); err != nil {
		return 255, err
	}
	terminal, terminalToken, err := openTerminalEndpoint(ctx, o)
	if err != nil {
		return 255, err
	}
	defer terminal.Close()
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
	tree := o.tree
	if tree == nil {
		tree = currentShellTree()
	}
	sourceMachine, sourceHost := localMachineIdentity()
	if err = streamMessage(control, message{Type: "attach", Token: terminalToken, OwnerProcess: describeSpawnOwner(o.owner), SourceHost: sourceHost, SourceMachineID: sourceMachine, Tree: tree, Instance: o.instance, Action: action, Cols: uint16(cols), Rows: uint16(rows), Term: termName, NoProfile: o.noProfile && (o.command == "launch" || o.command == "spawn"), NoLogin: o.noLogin && (o.command == "launch" || o.command == "spawn")}); err != nil {
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
	if (!o.local || o.hosted) && (!validTaskUID(m.Instance) || o.instance != "" && o.instance != m.Instance) {
		return 1, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if err := acceptSessionName(&o, m.Name); err != nil {
		return 1, err
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
	if o.reusable != nil {
		o.reusable.session, o.reusable.instance = o.session, o.instance
	}
	o.temporary = m.Temporary
	var participation *participatingShell
	var terminalStateConfirmed bool
	updateParticipation := func(state string) {
		if participation != nil {
			participation.Name, participation.State = o.session, state
			if err := setParticipatingShellState(*participation); err != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Could not update participating shell: %s\n", cleanText(err.Error()))
			}
		}
	}
	if !o.local {
		attachment := o.handoffAttachment
		if attachment == "" {
			attachment = newTaskUID()
		}
		row := participatingShell{Host: o.host, Instance: o.instance, Name: o.session, State: taskActive,
			Lifetime: shellLifetime(m.Temporary, true), Temporary: m.Temporary, PID: m.PID, Attachment: attachment}
		if err := observeParticipatingShell(row); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Could not record participating shell: %s\n", cleanText(err.Error()))
		} else {
			participation = &row
		}
		defer func() {
			if !terminalStateConfirmed {
				updateParticipation(taskUnknown)
			}
		}()
	}
	if err := removeRecentDetach(o); err != nil {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Could not update recent detaches: %s\n", cleanText(err.Error()))
	}
	if !o.local {
		if e := rememberConnection(o); e != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Connection history not saved: %s\n", cleanText(e.Error()))
		}
	}
	shellPID := m.PID

	confirmDetach := func(name string) {
		terminalStateConfirmed = true
		if validSession(name) == nil {
			o.session = name
		}
		updateParticipation(taskDetached)
		if err := rememberDetachedAttachment(o); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Detach history not saved: %s\n", cleanText(err.Error()))
		}
	}
	if o.detached {
		if err := detachAttachment(control, confirmDetach); err != nil {
			return 1, err
		}
		started := fmt.Sprintf("Started %s (%s).", cleanText(shellDisplayEndpoint(o)), shellTaskKind(o.temporary))
		fmt.Fprintln(os.Stdout, formatSessionNotice(os.Stdout, termui.Info, started))
		return 0, nil
	}
	if !o.local && !o.quiet {
		if err := showConnectedTasks(o); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Task list unavailable: %s\n", cleanText(err.Error()))
		}
	}
	restore, err := pty.RawConsole()
	if err != nil {
		return 1, err
	}
	defer func() { restore(); _ = pty.FlushInput() }()
	restoreTitle := pty.SetTitle("sshore: " + shellDisplayEndpoint(o))
	defer restoreTitle()
	var outputModes pty.InputState
	var console *commandConsole
	var escapeTimer *time.Timer
	var escapeTick <-chan time.Time
	defer func() {
		if escapeTimer != nil {
			escapeTimer.Stop()
		}
	}()
	defer outputModes.RestoreScreen()
	terminalOutput, flushOutput := pty.OutputWriter()
	defer func() { _ = flushOutput() }()
	closeConsole := func() error {
		if console == nil {
			return nil
		}
		c := console
		console = nil
		escapeTick = nil
		c.editor.Close()
		if err := outputModes.EndConsole(terminalOutput); err != nil {
			return err
		}
		if err := writeFull(terminalOutput, c.output); err != nil {
			return err
		}
		outputModes.Observe(c.output)
		return pty.SyncMouse(&outputModes)
	}
	defer func() { _ = closeConsole() }()

	events := make(chan clientEvent, 32)
	inputDrained := make(chan struct{}, 1)
	// The writer waits for each shortcut before sending the next one.
	keyHandled := make(chan struct{}, 1)
	emit := func(e clientEvent) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	}
	readAttachmentEvents(ctx, control, terminal, emit, inputDrained)
	stopKeyboard, err := pty.StartKeyboard(ctx, func(data []byte, err error) {
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
	// One terminal writer preserves keystroke order without making file I/O or a
	// stalled PTY block the main UI/control loop.
	inputQueue := make(chan []pty.KeyEvent, 16)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		var inputBytes uint64
		for {
			select {
			case <-ctx.Done():
				return
			case batch := <-inputQueue:
				for _, b := range batch {
					if b.Action != pty.KeyNone {
						if b.Action == pty.KeyDetach {
							emit(clientEvent{inputBarrier: true, inputOffset: inputBytes})
							// The control loop owns the detach deadline.
							select {
							case <-inputDrained:
							case <-ctx.Done():
								return
							}
						}
						emit(clientEvent{keyAction: b.Action})
						select {
						case <-keyHandled:
						case <-ctx.Done():
							return
						}
						if b.Action == pty.KeyDetach {
							return
						}
						continue
					}
					if err := writeFull(terminal, b.Data); err != nil {
						emit(clientEvent{err: err, inputFailure: true})
						return
					}
					inputBytes += uint64(len(b.Data))
				}
			}
		}
	}()
	defer func() {
		cancel()
		stopKeyboard()
		_ = terminal.Close()
		_ = transport.Close()
		<-inputDone
	}()
	canApprove := (!o.local || o.hosted) && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("TERM") != "dumb"
	if canApprove {
		if err := streamMessage(control, message{Type: "approval_ready"}); err != nil {
			return 255, err
		}
	}
	if o.handoffDone != nil {
		if err := o.handoffDone(ctx, nil); err != nil {
			return 1, err
		}
		o.handoffDone = nil
	}
	o.recovering = false
	interruptSignals.interactive.Store(true)
	attached = true
	from := ready(o)
	o.handoffHint = nil

	connected := "Connected " + cleanText(shellDisplayEndpoint(o))
	if from != "" {
		connected += " from " + cleanText(from)
	}
	if o.command != "attach" {
		connected += " (" + shellTaskKind(o.temporary) + ")"
	}
	sessionNotice(termui.Info, connected, "")
	color := termui.ColorEnabled(os.Stdout)
	notice := func(level termui.Color, format string, args ...any) {
		if console != nil {
			return
		}
		fmt.Fprint(os.Stdout, termui.Format(color, level, format, args...))
	}
	resizeInterval := 100 * time.Millisecond
	if runtime.GOOS == "windows" {
		// Keep the nested ConPTY close to the display size while dragging.
		resizeInterval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(resizeInterval)
	defer ticker.Stop()
	keys := pty.Keys{Bypass: o.noEscape}
	var keyTimer *time.Timer
	defer func() {
		if keyTimer != nil {
			keyTimer.Stop()
		}
	}()
	var keySequenceTimeout <-chan time.Time
	armKeyTimeout := func() {
		if !keys.Pending() {
			if keyTimer != nil {
				keyTimer.Stop()
			}
			keySequenceTimeout = nil
			return
		}
		if keyTimer == nil {
			keyTimer = time.NewTimer(50 * time.Millisecond)
		} else {
			keyTimer.Reset(50 * time.Millisecond)
		}
		keySequenceTimeout = keyTimer.C
	}
	const (
		detachIdle = iota
		drainingInput
		awaitingDetach
	)
	detachPhase := detachIdle
	var detachDeadline <-chan time.Time
	drainInput := func() {
		detachPhase = drainingInput
		detachDeadline = time.After(5 * time.Second)
	}
	queueDetach := func() bool {
		select {
		case inputQueue <- []pty.KeyEvent{{Action: pty.KeyDetach}}:
			drainInput()
			return true
		default:
			return false
		}
	}
	type approvalHandoff struct {
		request   requestRecord
		token     string
		confirmed bool
	}
	var approval *approvalHandoff
	var approvalDeadline <-chan time.Time
	sendDetach := func(inputUnconfirmed bool) error {
		if err := streamMessage(control, message{Type: "detach"}); err != nil {
			return err
		}
		if inputUnconfirmed {
			notice(termui.Warning, "\r\n[sshore] Input delivery unconfirmed; detaching.\r\n")
		}
		detachPhase = awaitingDetach
		detachDeadline = time.After(5 * time.Second)
		return nil
	}
	// A nonempty action awaits a reply, including the commit for a prepared
	// handoff. A target with no action is ready to use after detach confirms.
	type transitionRequest struct {
		action   string
		next     *sessionSwitch
		token    string
		deadline time.Time
		timeout  <-chan time.Time
	}
	var pending transitionRequest
	var consoleRequest uint64
	var openingConsole bool
	var openingConfirmation bool
	var openingInput []byte
	var handoffHint *handoffObservation
	defer func() {
		if handoffHint != nil {
			handoffHint.conn.Close()
		}
	}()
	clearTransition := func() { pending = transitionRequest{} }
	awaitResponse := func(delay time.Duration) {
		pending.deadline = time.Now().Add(delay)
		pending.timeout = time.After(time.Until(pending.deadline))
	}
	transitioning := func() bool { return pending.action != "" || pending.next != nil }
	actionForKey := func(action pty.KeyAction) string {
		switch action {
		case pty.KeyNew:
			return "spawn_child"
		case pty.KeyNext:
			return "next"
		case pty.KeyPrevious:
			return "previous"
		case pty.KeyLast:
			return "last"
		}
		return ""
	}
	queueSwitch := func(next *sessionSwitch) bool {
		if approval != nil || !queueDetach() {
			return false
		}
		pending = transitionRequest{next: next}
		return true
	}
	switchTo := func(selected sessionTarget) bool {
		target := o
		if o.routeParent != nil {
			target.routeTarget = true
		}
		target.command, target.session = "attach", selected.Name
		target.owner = processIdentity{}
		target.temporary = false
		target.instance, target.taskIdentity = selected.Instance, "task:"+selected.Instance
		target.hosted = o.local && selected.Hosted
		return queueSwitch(&sessionSwitch{to: target})
	}
	requestTargets := func(action string) error {
		awaitResponse(5 * time.Second)
		request := message{Type: "session_targets", Action: action}
		if o.local {
			current := o.session
			go func() {
				targets := localSessionTargets(current, request)
				emit(clientEvent{m: &targets})
			}()
			return nil
		}
		return streamMessage(control, request)
	}
	startChild := func(command string) error {
		pending.action = command + "_child"
		awaitResponse(30 * time.Second)
		return streamMessage(control, message{Type: pending.action})
	}
	requestConsoleTargets := func() error {
		consoleRequest++
		pending.action = fmt.Sprintf("console:%d", consoleRequest)
		return requestTargets(pending.action)
	}
	leaveConsole := func() error {
		if strings.HasPrefix(pending.action, "console:") {
			clearTransition()
		}
		if err := closeConsole(); err != nil {
			return err
		}
		return streamMessage(control, message{Type: "redraw"})
	}
	sendConsoleInput := func(data []byte) error {
		if console.editor.TryInput(data) {
			return nil
		}
		return leaveConsole()
	}
	interrupt := func() error {
		if transitioning() {
			if pending.next != nil && pending.next.to.handoffDone != nil {
				_ = pending.next.to.handoffDone(ctx, context.Canceled)
			}
			source := o
			source.command, source.recovering = "attach", true
			if detachPhase == detachIdle && !queueDetach() {
				return fmt.Errorf("terminal input queue is busy")
			}
			pending = transitionRequest{next: &sessionSwitch{to: source}}
			notice(termui.Info, "\r\n[sshore] Cancelled; returning to %s.\r\n", cleanText(o.session))
			return nil
		}
		if detachPhase != detachIdle {
			return nil
		}
		select {
		case inputQueue <- []pty.KeyEvent{{Data: []byte{3}}}:
			return nil
		default:
			return fmt.Errorf("terminal input queue is busy")
		}
	}
	for {
		select {
		case <-escapeTick:
			if console != nil {
				if err := leaveConsole(); err != nil {
					return 255, err
				}
			}
			escapeTick = nil
		case <-interruptSignals.interrupts:
			if console != nil {
				if err := leaveConsole(); err != nil {
					return 255, err
				}
				continue
			}
			if err := interrupt(); err != nil {
				return 1, err
			}
		case <-ctx.Done():
			return 130, ctx.Err()
		case <-keySequenceTimeout:
			keySequenceTimeout = nil
			batch := keys.FlushPending()
			if len(batch) > 0 && !transitioning() && approval == nil && detachPhase == detachIdle {
				select {
				case inputQueue <- batch:
				default:
					return 255, fmt.Errorf("terminal input stalled; closing connection; reconnect to check the shell")
				}
			}
		case <-pending.timeout:
			if strings.HasPrefix(pending.action, "console:inventory:") && console != nil {
				console.show("Inventory lookup timed out.")
			} else if strings.HasPrefix(pending.action, "console:") && console != nil {
				console.attach = ""
				console.show("Session lookup timed out.")
			} else if pending.action == "handoff" {
				if handoffHint != nil {
					handoffHint.conn.Close()
					handoffHint = nil
				}
				report := pending.next.to.handoffDone
				go func() { _ = report(ctx, context.DeadlineExceeded) }()
				notice(termui.Warning, "\r\n[sshore] Handoff timed out; staying here.\r\n")
			} else {
				notice(termui.Warning, "\r\n[sshore] Session lookup timed out; staying here.\r\n")
			}
			clearTransition()
		case <-approvalDeadline:
			approvalDeadline = nil
			approval = nil
		case <-detachDeadline:
			if detachPhase == awaitingDetach {
				return 255, fmt.Errorf("detach response timed out; shell preservation is unconfirmed")
			}
			if err = sendDetach(true); err != nil {
				return 255, err
			}
		case <-ticker.C:
			c, r, e := term.GetSize(int(os.Stdout.Fd()))
			if e == nil && (c != cols || r != rows) {
				cols, rows = c, r
				if err = streamMessage(control, message{Type: "resize", Cols: uint16(c), Rows: uint16(r)}); err != nil {
					return 255, err
				}
			}

		case e := <-events:
			if e.consoleSource != nil {
				if e.consoleSource != console {
					continue
				}
				switch {
				case e.console.Output != nil:
					if err := writeFull(terminalOutput, e.console.Output); err != nil {
						return 1, err
					}
				case e.console.Closed:
					if err := leaveConsole(); err != nil {
						return 255, err
					}
				case e.console.Line != nil:
					if console.confirm {
						if termui.ConfirmYes(*e.console.Line) {
							if !queueDetach() {
								console.showConfirmation("Terminal input queue is busy. Try again.")
								continue
							}
							clearTransition()
							if err := closeConsole(); err != nil {
								return 255, err
							}
						} else if err := leaveConsole(); err != nil {
							return 255, err
						}
						continue
					}
					args := strings.Fields(*e.console.Line)
					if len(args) > 0 {
						args[0] = canonicalCommand(args[0])
					}
					switch {
					case len(args) == 1 && args[0] == "detach":
						if detachPhase != detachIdle {
							break
						}
						if o.local && o.temporary && rootFrontend {
							clearTransition()
							console.showConfirmation("")
							break
						}
						if !queueDetach() {
							console.show("Terminal input queue is busy. Try again.")
							break
						}
						clearTransition()
						if err := closeConsole(); err != nil {
							return 255, err
						}
					case len(args) == 1 && args[0] == "exit":
						if err := leaveConsole(); err != nil {
							return 255, err
						}
					case len(args) == 1 && (args[0] == "spawn" || args[0] == "launch"):
						if err := closeConsole(); err != nil {
							return 1, err
						}
						if err := startChild(args[0]); err != nil {
							return 255, err
						}
					case len(args) == 2 && args[0] == "attach":
						console.attach = args[1]
						termui.WriteConsoleBody(terminalOutput, "Looking up session…")
						if err := requestConsoleTargets(); err != nil {
							return 255, err
						}
					case len(args) == 1 && (args[0] == "list" || args[0] == "ps" || args[0] == "pstree"):
						termui.WriteConsoleBody(terminalOutput, "Loading "+args[0]+"…")
						consoleRequest++
						pending.action = fmt.Sprintf("console:inventory:%d", consoleRequest)
						awaitResponse(requestTimeout)
						if err := streamMessage(control, message{Type: "console_inventory", Action: pending.action, Name: args[0], Color: color, Cols: uint16(cols)}); err != nil {
							return 255, err
						}
					default:
						console.show(consoleHelp)
					}
				}
				continue
			}
			if e.inputBarrier {
				if err := streamMessage(control, message{Type: "input_barrier", TerminalOffset: e.inputOffset}); err != nil {
					return 255, err
				}
				continue
			}
			if e.keyAction != pty.KeyNone {
				var keyErr error
				if detachPhase != awaitingDetach {
					switch e.keyAction {
					case pty.KeyNew, pty.KeyNext, pty.KeyPrevious, pty.KeyLast:
						action := actionForKey(e.keyAction)
						if detachPhase != detachIdle || approval != nil || action != pending.action {
							break
						}
						if e.keyAction == pty.KeyNew {
							keyErr = startChild("spawn")
						} else {
							keyErr = requestTargets(action)
						}
					case pty.KeyConsole:
						openingConsole = false
						if detachPhase != detachIdle || approval != nil || transitioning() {
							openingInput = nil
							break
						}
						if keyErr = outputModes.BeginConsole(terminalOutput); keyErr != nil {
							break
						}
						console = &commandConsole{}
						console.start(ctx, terminalOutput, emit, cols, color, openingConfirmation)
						if len(openingInput) > 0 {
							keyErr = sendConsoleInput(openingInput)
						}
						openingInput = nil
						if keyErr == nil && console != nil {
							if openingConfirmation {
								console.showConfirmation("")
							} else {
								keyErr = requestConsoleTargets()
							}
						}
						openingConfirmation = false
					case pty.KeyDetach:
						keyErr = sendDetach(false)
					case pty.KeyRedraw:
						keyErr = streamMessage(control, message{Type: "redraw"})
					}
				}
				keyHandled <- struct{}{}
				if keyErr != nil {
					return 255, keyErr
				}
				continue
			}
			if e.err != nil {
				if e.inputFailure && detachPhase != detachIdle {
					if detachPhase == drainingInput {
						if err = sendDetach(true); err != nil {
							return 255, err
						}
					}
					continue
				}
				return 255, fmt.Errorf("connection lost; reconnect to check the shell: %w", e.err)
			} else if len(e.output) > 0 {
				if detachPhase == awaitingDetach {
					continue
				}
				if console != nil {
					if len(console.output)+len(e.output) <= 256*1024 {
						console.output = append(console.output, e.output...)
						continue
					}
					if err := leaveConsole(); err != nil {
						return 255, err
					}
				}
				if err = writeFull(terminalOutput, e.output); err != nil {
					return 1, err
				}
				outputModes.Observe(e.output)
				if err = pty.SyncMouse(&outputModes); err != nil {
					return 1, fmt.Errorf("update terminal mouse mode: %w", err)
				}
			} else if e.input != nil {
				if openingConsole {
					if len(openingInput)+len(e.input) <= 64*1024 {
						openingInput = append(openingInput, e.input...)
					}
					continue
				}
				if console != nil {
					data := e.input
					if escapeTick != nil {
						escapeTimer.Stop()
						escapeTick = nil
						data = append([]byte{27}, data...)
					}
					if len(data) == 1 && data[0] == 27 {
						if escapeTimer == nil {
							escapeTimer = time.NewTimer(50 * time.Millisecond)
						} else {
							escapeTimer.Reset(50 * time.Millisecond)
						}
						escapeTick = escapeTimer.C
					} else if len(data) == 1 && (data[0] == 3 || data[0] == 4) {
						if err := leaveConsole(); err != nil {
							return 255, err
						}
					} else {
						if err := sendConsoleInput(data); err != nil {
							return 255, err
						}
					}
					continue
				}
				if transitioning() {
					// Keep paste boundaries across reads while ordinary input is
					// suppressed. A pasted Ctrl+C must not cancel the handoff.
					bypass := keys.Bypass
					keys.Bypass = false
					keys.Feed(e.input)
					armKeyTimeout()
					keys.Bypass = bypass
					if keys.Interrupted {
						if err := interrupt(); err != nil {
							return 1, err
						}
					}
					continue
				}
				if detachPhase != detachIdle || approval != nil {
					continue
				}
				batch := keys.Feed(e.input)
				if o.local && o.temporary && rootFrontend {
					for i := range batch {
						if batch[i].Action == pty.KeyDetach {
							batch[i].Action = pty.KeyConsole
							openingConfirmation = true
							break
						}
					}
				}
				armKeyTimeout()
				if len(batch) > 0 {
					select {
					case inputQueue <- batch:
						for _, key := range batch {
							if key.Action == pty.KeyConsole {
								openingConsole = true
								openingInput = append(openingInput[:0], key.Data...)
								break
							}
							if action := actionForKey(key.Action); action != "" {
								pending.action = action
							}
							if key.Action == pty.KeyDetach {
								drainInput()
								break
							}
						}
					default:
						return 255, fmt.Errorf("terminal input stalled; closing connection; reconnect to check the shell")
					}
				}

			} else if e.m != nil {
				switch e.m.Type {
				case "spawn_child_error":
					if pending.timeout != nil && (pending.action == "spawn_child" || pending.action == "launch_child") {
						clearTransition()
					}
					notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
				case "console_inventory":
					if console != nil && strings.HasPrefix(pending.action, "console:inventory:") && e.m.Action == pending.action {
						clearTransition()
						console.show(consoleInventoryBody(e.m.Output, rows))
					}
				case "session_targets":
					if pending.timeout == nil || pending.action == "handoff" || e.m.Action != pending.action {
						continue
					}
					clearTransition()
					if e.m.Error != "" {
						if console != nil && strings.HasPrefix(e.m.Action, "console:") {
							console.attach = ""
							console.show(e.m.Error)
							continue
						}
						notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
						continue
					}
					if validSession(e.m.Name) != nil {
						return 255, fmt.Errorf("invalid current session")
					}
					if err := validateSessionTargets(e.m.Targets); err != nil {
						return 255, err
					}
					o.session = e.m.Name
					if strings.HasPrefix(e.m.Action, "console:") {
						if console != nil {
							console.targets = e.m.Targets
							if console.attach == "" {
								console.show(consoleTargetSummary(console.targets, rows))
							} else {
								target, problem := resolveConsoleTarget(console.targets, console.attach)
								console.attach = ""
								if problem != "" {
									console.show(problem)
									continue
								}
								if err := closeConsole(); err != nil {
									return 1, err
								}
								if !switchTo(target) {
									notice(termui.Warning, "\r\n[sshore] terminal input queue is busy\r\n")
									if err := streamMessage(control, message{Type: "redraw"}); err != nil {
										return 255, err
									}
								}
							}
						}
						continue
					}
					var selected sessionTarget
					switch e.m.Action {
					case "next", "previous":
						selected = adjacentSessionTarget(o.session, e.m.Targets, e.m.Action == "previous")
						if selected.Name == "" {
							continue
						}
					case "last":
						for _, target := range e.m.Targets {
							if target.Instance == previous {
								selected = target
								break
							}
						}
						if selected.Instance == "" {
							notice(termui.Info, "\r\n[sshore] Previous session unavailable.\r\n")
							continue
						}
					}
					if selected.Instance == "" {
						return 255, fmt.Errorf("missing session target")
					}
					if !switchTo(selected) {
						notice(termui.Warning, "\r\n[sshore] terminal input queue is busy\r\n")
					}

				case "spawn_route":
					if pending.timeout != nil && (pending.action == "spawn_child" || pending.action == "launch_child") {
						clearTransition()
					}
					if o.local && !o.hosted || len(e.m.Token) != 64 {
						continue
					}
					parent := o
					token := e.m.Token
					reject := func() {
						go func() {
							_ = confirmSpawnRoute(ctx, parent, token, fmt.Errorf("client is switching sessions; try again"))
						}()
					}
					if detachPhase != detachIdle || transitioning() || console != nil {
						reject()
						continue
					}
					target := options{noLogin: e.m.NoLogin, noProfile: e.m.NoProfile, tree: e.m.Tree, command: "spawn", host: e.m.SourceHost, session: e.m.Name, temporary: true, routeParent: &parent, routeToken: e.m.Token, quiet: o.quiet}
					switch e.m.Action {
					case "local-launch":
						target.command, target.temporary = "launch", false
						fallthrough
					case "local":
						target.host = o.host
					}
					target.handoffDone = func(ctx context.Context, result error) error { return confirmSpawnRoute(ctx, parent, token, result) }
					if !queueSwitch(&sessionSwitch{to: target, returnToParent: true}) {
						reject()
					}
				case "handoff":
					if pending.timeout != nil && (pending.action == "spawn_child" || pending.action == "launch_child") {
						clearTransition()
					}
					// Only authenticated local control can redirect this frontend.
					if !o.local {
						return 1, fmt.Errorf("remote attachment requested local handoff")
					}
					target, handoffErr := e.m.Handoff.options()
					source, token := o, e.m.Token
					target.handoffAttachment = handoffAttachmentID(token)
					target.handoffDone = func(_ context.Context, result error) error {
						request := message{Type: "handoff_confirm", Token: token}
						if result != nil {
							request.Error = result.Error()
						}
						reply, err := localRequest("", source.taskIdentity, request)
						if err != nil {
							return err
						}
						if reply.Type != "handoff_confirmed" {
							return fmt.Errorf("handoff: %s", reply.Error)
						}
						return nil
					}
					if detachPhase != detachIdle || transitioning() || console != nil {
						handoffErr = fmt.Errorf("client is switching sessions")
					}
					if handoffErr == nil {
						handoffHint, handoffErr = openHandoffHint(source.taskIdentity, token)
					}
					reply := message{Type: "handoff_result", Token: e.m.Token}
					if handoffErr != nil {
						reply.Error = handoffErr.Error()
					} else {
						pending = transitionRequest{
							action: "handoff",
							next:   &sessionSwitch{to: target, returnToParent: true},
							token:  e.m.Token,
						}
						awaitResponse(5 * time.Second)
					}
					if err = streamMessage(control, reply); err != nil {
						return 255, err
					}
				case "handoff_commit":
					if !o.local {
						return 1, fmt.Errorf("remote attachment requested local handoff")
					}
					if pending.action != "handoff" || pending.token != e.m.Token || time.Now().After(pending.deadline) {
						continue
					}
					target := pending.next
					clearTransition()
					var handoffErr error
					if detachPhase != detachIdle {
						handoffErr = errors.New("remote handoff interrupted by session switch")
					} else if !queueSwitch(target) {
						handoffErr = errors.New("remote handoff failed: terminal input queue is busy")
					}
					if handoffErr != nil {
						if handoffHint != nil {
							handoffHint.conn.Close()
							handoffHint = nil
						}
						go func() { _ = target.to.handoffDone(ctx, handoffErr) }()
						notice(termui.Warning, "\r\n[sshore] %s.\r\n", handoffErr)
					}
				case "approval_request":
					if !canApprove || detachPhase != detachIdle || transitioning() || openingConsole || console != nil || approval != nil || len(e.m.Token) != 26 {
						if err := streamMessage(control, message{Type: "approval_rejected", Token: e.m.Token, Error: "client unavailable for approval"}); err != nil {
							return 255, err
						}
						continue
					}
					request, approvalErr := newApprovalRequest(o, shellPID, e.m.Operations)
					if approvalErr != nil {
						if err := streamMessage(control, message{Type: "approval_rejected", Token: e.m.Token, Error: approvalErr.Error()}); err != nil {
							return 255, err
						}
						continue
					}
					approval = &approvalHandoff{request: request, token: e.m.Token}
					approvalDeadline = time.After(requestTimeout)
					if err := streamMessage(control, message{Type: "approval_handoff", Token: e.m.Token}); err != nil {
						return 255, err
					}
				case "approval_cancelled":
					if approval != nil && approval.token == e.m.Token && !approval.confirmed {
						approval, approvalDeadline = nil, nil
					}
				case "approval_ack":
					if approval == nil || approval.token != e.m.Token || approval.confirmed || detachPhase != detachIdle {
						continue
					}
					if !queueDetach() {
						return 1, fmt.Errorf("terminal input queue is busy; request the operation again")
					}
					approval.confirmed, approvalDeadline = true, nil
				case "detach_requested":
					if err := closeConsole(); err != nil {
						return 1, err
					}
					if detachPhase == detachIdle {
						if err = sendDetach(false); err != nil {
							return 255, err
						}
					}
				case "detached":
					confirmDetach(e.m.Name)
					_ = streamMessage(control, message{Type: "bye"})
					if approval != nil && approval.confirmed {
						return 0, &localApproval{request: approval.request, source: o}
					}
					if pending.action == "" && pending.next != nil {
						pending.next.from = returnTarget{options: o, previous: previous}
						if pending.next.returnToParent && handoffHint != nil {
							pending.next.from.options.handoffHint = handoffHint
							handoffHint = nil
						}
						return 0, pending.next
					}
					ended = func() {
						if o.temporary {
							sessionNotice("", "Backgrounded "+cleanText(shellDisplayEndpoint(o))+" until its parent shell exits", "Return: "+cleanText(reconnectCommand(o)))
						} else {
							sessionNotice("", "Detached from "+cleanText(shellDisplayEndpoint(o))+" (still running)", "Reconnect: "+cleanText(reconnectCommand(o)))
						}
					}
					return 0, nil
				case "exit":
					terminalStateConfirmed = true
					if exitErr := rememberShellExit(o); exitErr != nil {
						notice(termui.Warning, "\r\n[sshore] Could not record shell exit: %s\r\n", cleanText(exitErr.Error()))
					}
					var level termui.Color
					if e.m.Code != 0 {
						level = termui.Error
					}
					exit := e.m
					ended = func() {
						reason := cleanText(exit.Reason)
						title := fmt.Sprintf("Exited %s (code %d)", cleanText(shellDisplayEndpoint(o)), exit.Code)
						if reason != "" && !strings.HasPrefix(reason, "shell exited (code ") {
							title += ": " + reason
						}
						sessionNotice(level, title, "")
					}
					return e.m.Code, nil
				case "fatal":
					return 1, errors.New(e.m.Error)
				}
			}
		}
	}
}

// A disconnected transport does not establish that the hosted shell exited.
func sessionDisconnectReason(err error) string {
	switch timeout, isTimeout := errors.AsType[net.Error](err); {
	case errors.Is(err, context.DeadlineExceeded), isTimeout && timeout.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return "connection closed"
	default:
		// Local phase timers currently return contextual errors rather than net.Error.
		if strings.Contains(err.Error(), "timed out") {
			return "timeout"
		}
		return "connection error"
	}
}

func sessionNotice(level termui.Color, title, detail string) {
	title = formatSessionNotice(os.Stderr, level, title)
	if detail != "" {
		title += "\r\n  " + detail
	}
	fmt.Fprintf(os.Stderr, "\r\n%s\r\n", title)
}

func formatSessionNotice(f *os.File, level termui.Color, title string) string {
	if level != "" && termui.ColorEnabled(f) {
		word, rest, _ := strings.Cut(title, " ")
		title = termui.Render(true, level, word) + " " + rest
	}
	return "[sshore] " + title
}

// Retain transport options in the reconnect hint, including custom ports and keys.
// Use POSIX shell quoting on Linux and PowerShell quoting on Windows.
func shellDisplayEndpoint(o options) string {
	if o.local {
		return localEndpointPrefix + o.session
	}
	return transportio.FormatEndpoint(o.host, o.session)
}

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
	selector := o.session
	if validTaskUID(o.instance) {
		selector = o.instance
	}
	args := []string{"sshore", "attach", transportio.FormatEndpoint(o.host, selector)}
	if o.local {
		identity := o.taskIdentity
		if identity == "" {
			identity = identityFromUID(o.instance)
		}
		selector := taskUID(identity)
		if selector == "" {
			selector = o.session
		}
		args = []string{"sshore", "attach", localEndpointPrefix + selector}
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
