package main

// The frontend owns terminal interaction; daemon-owned tasks outlive its
// connections. Remote operation requests are approved in the local terminal.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/taskid"
	xterm "golang.org/x/term"

	"github.com/jay-waves/sshore/internal/shell"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/term"
	"github.com/jay-waves/sshore/internal/termui"
	bolt "go.etcd.io/bbolt"
)

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

const terminalCloseGrace = 2 * time.Second

type clientEvent struct {
	consoleSource     *commandConsole
	consoleGeneration uint64
	console           termui.ConsoleEvent
	filePicker        *consoleFilePicker
	fileOutput        []byte
	fileRequest       *consoleFileRequest
	fileResult        *consoleFileResult
	inputBarrier      bool
	inputOffset       uint64
	keyAction         term.KeyAction
	inputFailure      bool
	outputIncomplete  bool
	m                 *message
	input             []byte
	output            []byte
	err               error
}

// Drain the terminal tail before publishing a confirmed exit. A data-stream
// failure can truncate the tail without invalidating the control frame.
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
			drain:
				for outputBytes.Load() < m.TerminalOffset {
					select {
					case <-outputProgress:
						deadline.Reset(5 * time.Second)
					case <-deadline.C:
						break drain
					case <-outputDone:
						break drain
					case <-ctx.Done():
						deadline.Stop()
						return
					}
				}
				deadline.Stop()
			}
			if m.Type == "input_drained" {
				select {
				case inputDrained <- struct{}{}:
				case <-ctx.Done():
					return
				}
				continue
			}
			outputIncomplete := m.Type == "exit" && (m.OutputIncomplete || outputBytes.Load() < m.TerminalOffset)
			emit(clientEvent{m: &m, outputIncomplete: outputIncomplete})
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
				_ = control.SetReadDeadline(time.Now().Add(terminalCloseGrace))
				return
			}
		}
	}()
}

// Learn the current shell name before saving approval metadata.
func acceptSessionName(o *options, name string) error {
	if o.session != "" && o.command != "attach" {
		return nil
	}
	if err := validSession(name); err != nil {
		return fmt.Errorf("server returned an invalid shell name: %w", err)
	}
	o.session = name
	return nil
}

type sessionEnd struct {
	title, detail       string
	level               termui.Color
	exit                bool
	localDaemonShutdown bool
	retained            []retainedShell
}

func runClientOnce(o options, transports clientTransports, panel *clientConsole, end *sessionEnd, ready func(options) (from, parentID string)) (resultCode int, resultErr error) {
	attached := false
	var ended func()
	report := func(level termui.Color, title, detail string) {
		end.level, end.title, end.detail = level, title, detail
		if !end.exit {
			sessionNotice(level, title, detail)
		}
	}
	// Report after input shutdown and console restoration, including nested returns.
	defer func() {
		if ended != nil {
			ended()
			return
		}
		_, switching := errors.AsType[*sessionSwitch](resultErr)
		_, approving := errors.AsType[*localApproval](resultErr)
		if attached && resultErr != nil && !switching && !approving {
			report(termui.Warning, fmt.Sprintf("Disconnected from %s (%s). Shell state unconfirmed.", cleanText(shellDisplayEndpoint(o)), sessionDisconnectReason(resultErr)), "")
		}
	}()
	signalCtx, interruptSignals, stopSignals := newClientSignals(o.recovering)
	defer stopSignals()
	// Cancel I/O before restoring the console, but keep signal handling installed
	// until the keyboard has stopped and terminal restoration has completed.
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	if !xterm.IsTerminal(int(os.Stdin.Fd())) || !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return 1, fmt.Errorf("an interactive terminal is required")
	}
	term.ResetInputModes()
	control, err := openClientTransportContext(ctx, &o, transports)
	if err != nil {
		return 1, fmt.Errorf("open shell connection: %w", err)
	}
	defer control.Close()
	terminalStream, terminalToken, err := openTerminalEndpoint(ctx, o)
	if err != nil {
		return 255, fmt.Errorf("open shell terminal: %w", err)
	}
	defer terminalStream.Close()
	cols, rows, err := xterm.GetSize(int(os.Stdout.Fd()))
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
	sourceMachine, sourceHost := localMachineIdentity()
	treeKey := o.fromTree
	if o.hold != nil {
		treeKey = o.hold.treeKey
	}
	var argv []string
	if o.command == "sh" {
		argv = o.argv
	}
	if err = streamMessage(control, message{Type: "attach", SoftwareVersion: version, TreeKey: treeKey, Token: terminalToken, Pinned: o.command == "sh" && o.pin, SourceHost: sourceHost, MachineID: sourceMachine, Instance: o.instance, Action: action, terminalDisplay: terminalDisplay{Cols: uint16(cols), Rows: uint16(rows), Term: termName}, NoProfile: o.noProfile && o.command == "sh", NoLogin: o.noLogin && o.command == "sh", Argv: argv}); err != nil {
		return 255, err
	}
	m, err := readMessage(control)
	if err != nil {
		return 255, fmt.Errorf("read shell readiness: %w", err)
	}
	if m.Type == "fatal" {
		_ = streamMessage(control, message{Type: "bye"})
		return 1, m.peerError("shell daemon")
	}
	if m.Type != "ready" {
		return 1, fmt.Errorf("unexpected attach handshake")
	}
	if err := checkPeerVersion(m.SoftwareVersion, "shell daemon"); err != nil {
		return 1, err
	}
	if (!o.local || o.hosted) && (!taskid.Valid(m.Instance) || o.instance != "" && o.instance != m.Instance) {
		return 1, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if err := acceptSessionName(&o, m.Name); err != nil {
		return 1, err
	}
	if o.hold != nil {
		o.hold.close()
		delete(transports, o.hold)
		o.hold = nil
	}
	o.fromTree = ""
	attachmentKey := m.TreeKey
	if warn, err := observeIdentityRoute(m.MachineID, o.host, o.ssh); err != nil {
		termui.Log(os.Stderr, "", termui.Warning, "[sshore] Identity tracking failed: %s\n", cleanText(err.Error()))
	} else if warn {
		termui.Log(os.Stderr, "", termui.Warning, "[sshore] Same identity through a new entry point; if this is a clone, reset that host's sshore state.\n")
	}
	o.instance = m.Instance
	o.remoteOwned = !o.local && m.Owner != ""
	if o.reusable != nil {
		o.reusable.session, o.reusable.instance = o.session, o.instance
	}
	var participation *participatingShell
	var terminalStateConfirmed bool
	if !o.local {
		participation = &participatingShell{HostInfo: o.hostInfo, Host: o.host, Owner: m.Owner, Instance: o.instance, Name: o.session, State: taskActive,
			Pinned: m.Pinned, PinUntil: m.PinUntil, PID: m.PID, Shell: m.Shell, Program: m.Program, Attachment: taskid.New()}
		if err := updateHistory(func(tx *bolt.Tx) error {
			if err := removeRecentDetach(tx, o.instance); err != nil {
				return err
			}
			return rememberRemoteAttachment(tx, o, *participation)
		}); err != nil {
			termui.Log(os.Stderr, "", termui.Warning, "[sshore] Could not save shell history: %s\n", cleanText(err.Error()))
			participation = nil
		}
	}
	if participation != nil {
		defer func() {
			if !terminalStateConfirmed {
				participation.State = taskUnknown
				if err := updateHistory(func(tx *bolt.Tx) error {
					return setParticipationState(tx, *participation)
				}); err != nil {
					termui.Log(os.Stderr, "", termui.Warning, "[sshore] Could not update shell history: %s\n", cleanText(err.Error()))
				}
			}
		}()
	}
	shellPID := m.PID

	confirmDetach := func(reply message) {
		terminalStateConfirmed = true
		if validSession(reply.Name) == nil {
			o.session = reply.Name
		}
		if participation == nil {
			return
		}
		participation.Name, participation.State = o.session, taskDetached
		participation.Pinned, participation.PinUntil = reply.Pinned, reply.PinUntil
		if o.hold != nil {
			participation.State = taskWaiting
		}
		if err := updateHistory(func(tx *bolt.Tx) error {
			return setParticipationState(tx, *participation)
		}); err != nil {
			termui.Log(os.Stderr, "", termui.Warning, "[sshore] Could not save detach history: %s\n", cleanText(err.Error()))
		}
	}
	restore, err := term.RawConsole()
	if err != nil {
		return 1, err
	}
	defer func() { restore(); _ = term.FlushInput() }()
	term.ResetInputModes()
	restoreTitle := term.SetTitle("sshore: " + shellDisplayEndpoint(o))
	defer restoreTitle()
	var outputModes term.InputState
	var console *commandConsole
	var picker *consoleFilePicker
	var escapeTimer *time.Timer
	var escapeTick <-chan time.Time
	defer func() {
		if escapeTimer != nil {
			escapeTimer.Stop()
		}
	}()
	defer outputModes.RestoreScreen()
	terminalOutput, flushOutput := term.OutputWriter()
	defer func() { _ = flushOutput() }()
	closeConsole := func() error {
		if console == nil {
			return nil
		}
		c := console
		console = nil
		escapeTick = nil
		if picker != nil {
			picker.cancel()
			picker = nil
		}
		c.stopEditor()
		if err := outputModes.EndConsole(terminalOutput); err != nil {
			return err
		}
		if err := writeFull(terminalOutput, c.output); err != nil {
			return err
		}
		outputModes.Observe(c.output)
		return term.SyncMouse(&outputModes)
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
	readAttachmentEvents(ctx, control, terminalStream, emit, inputDrained)
	stopKeyboard, err := term.StartKeyboard(ctx, func(data []byte, err error) {
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
	inputQueue := make(chan []term.KeyEvent, 16)
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
					if b.Action != term.KeyNone {
						if b.Action == term.KeyDetach {
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
						if b.Action == term.KeyDetach {
							return
						}
						continue
					}
					if err := writeFull(terminalStream, b.Data); err != nil {
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
		_ = terminalStream.Close()
		_ = control.Close()
		<-inputDone
	}()
	canApprove := (!o.local || o.hosted) && xterm.IsTerminal(int(os.Stdin.Fd())) && xterm.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("TERM") != "dumb"
	if canApprove {
		if err := streamMessage(control, message{Type: "approval_ready"}); err != nil {
			return 255, err
		}
	}
	if o.handoffDone != nil {
		if err := o.handoffDone(ctx, nil); err != nil {
			return 1, fmt.Errorf("confirm shell handoff: %w", err)
		}
		o.handoffDone = nil
	}
	o.recovering = false
	interruptSignals.interactive.Store(true)
	attached = true
	from, parentID := ready(o)

	connected := "Connected " + cleanText(shellDisplayEndpoint(o))
	if from != "" {
		connected += " from " + cleanText(from)
	}
	if o.command != "attach" {
		connected += " (sh)"
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
	keys := term.Keys{Bypass: o.noEscape}
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
	detachRequest := message{Type: "detach"}
	explicitDetach := false
	queueDetach := func(m message) bool {
		select {
		case inputQueue <- []term.KeyEvent{{Action: term.KeyDetach}}:
			detachRequest = m
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
		if detachRequest.Action == "switch" && !explicitDetach && o.hold == nil {
			hold, err := holdClientTree(o, attachmentKey, transports)
			if err != nil {
				return err
			}
			o.hold = hold
		}
		if err := streamMessage(control, detachRequest); err != nil {
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
	var openingConsole, inputClosed bool
	var openingInput []byte
	clearTransition := func() { pending = transitionRequest{} }
	detachFromConsole := func(request message) error {
		if !queueDetach(request) {
			console.show("Terminal input queue is busy. Try again.")
			return nil
		}
		clearTransition()
		return closeConsole()
	}
	awaitResponse := func(delay time.Duration) {
		pending.deadline = time.Now().Add(delay)
		pending.timeout = time.After(time.Until(pending.deadline))
	}
	transitioning := func() bool { return pending.action != "" || pending.next != nil }
	queueSwitch := func(next *sessionSwitch) bool {
		if approval != nil || !queueDetach(message{Type: "detach", Action: "switch"}) {
			return false
		}
		pending = transitionRequest{next: next}
		return true
	}
	switchTo := func(selected sessionTarget) bool {
		target := o
		if o.routeParent != nil {
			target.routeTarget = true
		} else if !o.local && o.parent != "" && o.reusable != nil {
			parent := o
			target.routeParent, target.routeTarget, target.sameHostRoute = &parent, true, true
		}
		target.command, target.session = "attach", selected.Name
		target.instance, target.taskIdentity = selected.Instance, "task:"+selected.Instance
		target.hosted = o.local && selected.Hosted
		target.argv = nil
		return queueSwitch(&sessionSwitch{to: target, sameDaemon: true})
	}
	startChild := func(pin bool, directory string) error {
		pending.action = "sh_child"
		awaitResponse(30 * time.Second)
		return streamMessage(control, message{Type: pending.action, Pinned: pin, Directory: directory})
	}
	requestSessionTargets := func(purpose, ref string) error {
		consoleRequest++
		pending.action = fmt.Sprintf("%s%d", purpose, consoleRequest)
		awaitResponse(5 * time.Second)
		return streamMessage(control, message{Type: "session_targets", Action: pending.action, Name: ref, NativeTargets: o.local && o.routeParent == nil})
	}
	releaseOwnedTransport := func() {
		if t := o.reusable; t != nil && t.ownsShell {
			t.ownsShell = false
			delete(transports, t)
			t.close()
		}
	}
	requestConsoleInventory := func(name string) error {
		consoleRequest++
		pending.action = fmt.Sprintf("console:inventory:%d", consoleRequest)
		awaitResponse(requestTimeout)
		return streamMessage(control, message{Type: "console_inventory", Action: pending.action, Name: name, terminalDisplay: terminalDisplay{Color: color, Cols: uint16(cols)}})
	}
	leaveConsole := func() error {
		if strings.HasPrefix(pending.action, "console:") || strings.HasPrefix(pending.action, consoleAttachLookup) {
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
			if detachPhase == detachIdle && !queueDetach(message{Type: "detach", Action: "switch"}) {
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
		case inputQueue <- []term.KeyEvent{{Data: []byte{3}}}:
			return nil
		default:
			return fmt.Errorf("terminal input queue is busy")
		}
	}
	// A remote frame proves the attachment was seen. Sample it off the terminal I/O path.
	var nextObservation time.Time
	var observations sync.WaitGroup
	defer observations.Wait()
	for {
		select {
		case <-escapeTick:
			if console != nil {
				console.clearInput()
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
			} else if console != nil {
				console.show("Session lookup timed out.")
			} else if pending.action == "handoff" {
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
			c, r, e := xterm.GetSize(int(os.Stdout.Fd()))
			if e == nil && (c != cols || r != rows) {
				cols, rows = c, r
				if err = streamMessage(control, message{Type: "resize", terminalDisplay: terminalDisplay{Cols: uint16(c), Rows: uint16(r)}}); err != nil {
					return 255, err
				}
			}

		case e := <-events:
			if e.filePicker != nil {
				if e.filePicker != picker {
					continue
				}
				switch {
				case e.fileOutput != nil:
					if err := writeFull(terminalOutput, e.fileOutput); err != nil {
						return 1, err
					}
				case e.fileRequest != nil:
					if e.fileRequest.ctx.Err() != nil {
						continue
					}
					consoleRequest++
					action := strconv.FormatUint(consoleRequest, 10)
					picker.requests[action] = e.fileRequest
					if err := streamMessage(control, message{Type: "console_files", Action: action, Directory: e.fileRequest.dir, ConsoleFiles: &consoleFilePayload{ParentDirectory: e.fileRequest.parent}}); err != nil {
						return 255, err
					}
				case e.fileResult != nil:
					if e.fileResult.err != nil {
						picker.cancel()
						picker = nil
						if err := console.resumeAfterPicker(terminalOutput, e.fileResult.err); err != nil {
							return 1, err
						}
					} else {
						if err := closeConsole(); err != nil {
							return 1, err
						}
						if err := startChild(false, e.fileResult.path); err != nil {
							return 255, err
						}
					}
				}
				continue
			}
			if e.consoleSource != nil {
				if !console.accepts(e) {
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
					command, ok := console.readCommand(*e.console.Line, panel.directory)
					if !ok {
						break
					}
					switch command.command {
					case "list", "pstree":
						if err := requestConsoleInventory(command.command); err != nil {
							return 255, err
						}
					case "detach":
						request, err := detachMessage(command)
						if err != nil {
							console.show(err.Error())
							break
						}
						if err := detachFromConsole(request); err != nil {
							return 255, err
						}
					case "exit":
						if err := leaveConsole(); err != nil {
							return 255, err
						}
					case "lf":
						if panel.directory == "" {
							console.show("Current directory is unavailable.")
							break
						}
						host := o.host
						if o.local {
							host = termui.LocalFileHost()
						}
						escapeTick = nil
						console.stopEditor()
						picker = startConsoleFilePicker(ctx, panel.directory, host, true, emit)
					case "shell":
						if command.remoteCommand != "" || command.task.scope != taskLocal || command.startupDirectory != "" || len(command.values) > 1 || len(command.values) == 1 && !command.has("--pin") {
							console.show("Usage: shell [--pin]")
							break
						}
						if err := closeConsole(); err != nil {
							return 1, err
						}
						if err := startChild(command.has("--pin"), ""); err != nil {
							return 255, err
						}
					case "attach":
						ref := cmp.Or(command.task.ref, command.operand(0))
						if ref != "" && ref == parentID {
							if err := detachFromConsole(message{Type: "detach"}); err != nil {
								return 255, err
							}
							break
						}
						lookup := consoleAttachLookup
						if taskid.IsPrefix(ref) && strings.HasPrefix(parentID, ref) {
							lookup = consoleParentLookup
						}
						if err := requestSessionTargets(lookup, ref); err != nil {
							return 255, err
						}
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
			if e.keyAction != term.KeyNone {
				var keyErr error
				if detachPhase != awaitingDetach {
					switch e.keyAction {
					case term.KeyNew:
						if detachPhase == detachIdle && approval == nil && pending.action == "sh_child" {
							keyErr = startChild(false, "")
						}
					case term.KeyAttach:
						if detachPhase == detachIdle && approval == nil && !transitioning() {
							keyErr = requestSessionTargets(consoleAttachLookup, "")
						}
					case term.KeyConsole:
						openingConsole = false
						if detachPhase != detachIdle || approval != nil || transitioning() {
							openingInput = nil
							break
						}
						if keyErr = outputModes.BeginConsole(terminalOutput); keyErr != nil {
							break
						}
						host := o.host
						if o.local {
							host = "local"
						}
						panel.directory = ""
						console = openCommandConsole(ctx, emit, host, cols, color, true)
						if len(openingInput) > 0 {
							keyErr = sendConsoleInput(openingInput)
						}
						openingInput = nil
						if keyErr == nil && console != nil {
							keyErr = requestSessionTargets(consoleTargetLookup, "")
						}
					case term.KeyDetach:
						keyErr = sendDetach(false)
					case term.KeyRedraw:
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
				if e.inputFailure {
					inputClosed = true
					switch detachPhase {
					case detachIdle:
						_ = control.SetReadDeadline(time.Now().Add(terminalCloseGrace))
					case drainingInput:
						if err = sendDetach(true); err != nil {
							return 255, err
						}
					}
					continue
				}
				return 255, fmt.Errorf("connection lost; reconnect to check the shell: %w", e.err)
			} else if len(e.output) > 0 {
				if participation != nil && time.Now().After(nextObservation) {
					seen := time.Now()
					nextObservation = seen.Add(time.Minute)
					row := *participation
					observations.Go(func() {
						_ = touchParticipation(row, seen)
					})
				}
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
				if err = term.SyncMouse(&outputModes); err != nil {
					return 1, fmt.Errorf("update terminal mouse mode: %w", err)
				}
			} else if e.input != nil && !inputClosed {
				if openingConsole {
					if len(openingInput)+len(e.input) <= 64*1024 {
						openingInput = append(openingInput, e.input...)
					}
					continue
				}
				if console != nil {
					if picker != nil {
						picker.send(e.input)
						continue
					}
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
				armKeyTimeout()
				if len(batch) > 0 {
					select {
					case inputQueue <- batch:
						for _, key := range batch {
							if key.Action == term.KeyConsole {
								openingConsole = true
								openingInput = append(openingInput[:0], key.Data...)
								break
							}
							if key.Action == term.KeyNew {
								pending.action = "sh_child"
							}
							if key.Action == term.KeyDetach {
								detachRequest = message{Type: "detach"}
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
				case "console_files":
					if picker != nil {
						if request := picker.requests[e.m.Action]; request != nil {
							delete(picker.requests, e.m.Action)
							request.reply <- *e.m
						}
					}
				case "sh_child_error":
					if pending.timeout != nil && pending.action == "sh_child" {
						clearTransition()
					}
					notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
				case "console_inventory":
					if console != nil && strings.HasPrefix(pending.action, "console:inventory:") && e.m.Action == pending.action {
						clearTransition()
						console.show(consoleInventoryBody(e.m.Output, rows))
					}
				case "session_targets":
					if pending.timeout == nil || e.m.Action != pending.action {
						continue
					}
					clearTransition()
					if e.m.Error != "" {
						if console != nil {
							console.show(e.m.Error)
						} else {
							notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
						}
						continue
					}
					if validSession(e.m.Name) != nil {
						return 255, fmt.Errorf("invalid current session")
					}
					if err := validateSessionTargets(e.m.Targets); err != nil {
						return 255, err
					}
					o.session = e.m.Name
					if strings.HasPrefix(e.m.Action, consoleTargetLookup) {
						if console != nil {
							console.targets = e.m.Targets
							if e.m.Directory != "" {
								panel.directory = e.m.Directory
								if o.local {
									panel.localDirectory = panel.directory
								}
							}
							console.show("")
						}
						continue
					}
					if strings.HasPrefix(e.m.Action, consoleParentLookup) {
						if len(e.m.Targets) != 0 && e.m.Targets[0].Instance != parentID {
							console.show("Ambiguous task ID. Use the full task ID to return to the parent.")
						} else if err := detachFromConsole(message{Type: "detach"}); err != nil {
							return 255, err
						}
						continue
					}
					if len(e.m.Targets) == 0 {
						const message = "No detached session on this host."
						if console != nil {
							console.show(message)
						} else {
							notice(termui.Info, "\r\n[sshore] %s\r\n", message)
						}
						continue
					}
					if err := closeConsole(); err != nil {
						return 1, err
					}
					if !switchTo(e.m.Targets[0]) {
						notice(termui.Warning, "\r\n[sshore] terminal input queue is busy\r\n")
						if err := streamMessage(control, message{Type: "redraw"}); err != nil {
							return 255, err
						}
					}
				case "shell_route":
					if pending.timeout != nil && pending.action == "sh_child" {
						clearTransition()
					}
					if o.local && !o.hosted || len(e.m.Token) != 64 || len(e.m.HandoffToken) != 64 {
						continue
					}
					parent := o
					token, handoffToken := e.m.Token, e.m.HandoffToken
					reject := func() {
						go func() {
							_ = confirmShellRoute(ctx, parent, token, handoffToken, fmt.Errorf("client is switching sessions; try again"))
						}()
					}
					if detachPhase != detachIdle || transitioning() || console != nil {
						reject()
						continue
					}
					target := options{hostInfo: e.m.advertisedHost(), argv: e.m.Argv, pin: e.m.Pinned, noLogin: e.m.NoLogin, noProfile: e.m.NoProfile, command: "sh", host: e.m.SourceHost, session: e.m.Name, instance: e.m.Instance, routeParent: &parent, routeToken: e.m.Token}
					if e.m.Action == "attach" || e.m.Action == "remote_attach" {
						target.command = "attach"
					}
					if e.m.Action == "local" || e.m.Action == "attach" {
						target.host, target.sameHostRoute, target.hostInfo = o.host, true, o.hostInfo
					}
					target.handoffDone = func(ctx context.Context, result error) error {
						return confirmShellRoute(ctx, parent, token, handoffToken, result)
					}
					if !queueSwitch(&sessionSwitch{to: target, sameDaemon: target.sameHostRoute}) {
						reject()
					}
				case "handoff":
					if pending.timeout != nil && pending.action == "sh_child" {
						clearTransition()
					}
					// Only authenticated local control can redirect this frontend.
					if !o.local {
						return 1, fmt.Errorf("remote attachment requested local handoff")
					}
					target, handoffErr := e.m.Handoff.options()
					source, token := o, e.m.Token
					if target.command == "sh" || !target.local {
						target.parent = taskid.ParseKey(source.taskIdentity)
					}
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
					reply := message{Type: "handoff_result", Token: e.m.Token}
					if handoffErr != nil {
						reply.Error = handoffErr.Error()
					} else {
						pending = transitionRequest{
							action: "handoff",
							next:   &sessionSwitch{to: target, sameDaemon: target.local},
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
					if err := streamMessage(control, message{Type: "approval_handoff", Token: e.m.Token, ApprovalID: request.ID}); err != nil {
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
					if !queueDetach(message{Type: "detach", Action: "switch"}) {
						return 1, fmt.Errorf("terminal input queue is busy; request the operation again")
					}
					approval.confirmed, approvalDeadline = true, nil
				case "pin_failed":
					detachPhase, detachDeadline = detachIdle, nil
					if console != nil {
						console.show(cleanText(e.m.Error))
					} else {
						notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
					}
				case "detach_requested":
					explicitDetach = true
					detachRequest = message{Type: "detach", Action: "switch"}
					if err := closeConsole(); err != nil {
						return 1, err
					}
					if detachPhase == detachIdle {
						if err = sendDetach(false); err != nil {
							return 255, err
						}
					}
				case "detached":
					confirmDetach(*e.m)
					_ = streamMessage(control, message{Type: "bye"})
					if approval != nil && approval.confirmed {
						return 0, &localApproval{request: approval.request, source: o}
					}
					if pending.action == "" && pending.next != nil {
						pending.next.from = o
						dest := &pending.next.to
						// Keep the old reservation for rollback. A cycle commits to an independent tree.
						if o.hold != nil {
							switch {
							case dest.recovering:
								dest.hold = o.hold
							case pending.next.sameDaemon:
								dest.fromTree = o.hold.treeKey
							case dest.parent != "":
								dest.remoteParentKey = o.hold.treeKey
							}
						}
						return 0, pending.next
					}
					if detachRequest.Action != "switch" || explicitDetach {
						releaseOwnedTransport()
					}
					retention := pinDetail("", e.m.Pinned, e.m.PinUntil)
					ended = func() {
						title := "Detached from " + cleanText(shellDisplayEndpoint(o))
						if retention != "" {
							title += " (" + retention + ")"
						}
						hint, target := "Reconnect: ", o
						if m.Owner != "" {
							hint = "Reconnect from its parent: "
							target.local = target.local || target.sameHostRoute
							target.routeParent, target.ssh = nil, nil
						}
						report("", title, hint+cleanText(reconnectCommand(target)))
					}
					return 0, nil
				case "exit":
					terminalStateConfirmed = true
					releaseOwnedTransport()
					var level termui.Color
					if e.m.Code != 0 {
						level = termui.Error
					}
					exit := e.m
					end.exit, end.retained = true, exit.RetainedShells
					end.localDaemonShutdown = o.local && exit.DaemonShutdown
					ended = func() {
						reason := cleanText(exit.Reason)
						title := fmt.Sprintf("Exited %s (code %d)", cleanText(shellDisplayEndpoint(o)), exit.Code)
						if reason != "" && reason != "shell exited" && reason != fmt.Sprintf("exit status %d", exit.Code) &&
							!strings.HasPrefix(reason, "shell exited (code ") {
							title += ": " + reason
						}
						detail := ""
						if e.outputIncomplete {
							detail = "Final terminal output was incomplete."
						}
						report(level, title, detail)
						if end.localDaemonShutdown {
							return
						}
						if exitErr := rememberShellExit(o); exitErr != nil {
							termui.Log(os.Stderr, "", termui.Warning, "[sshore] Could not record shell exit: %s\n", cleanText(exitErr.Error()))
						}
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
	o = directShellOptions(o)
	quote := func(s string) string {
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%_+=:,./-", r))
		}) < 0 {
			return s
		}
		if runtime.GOOS == "windows" {
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		}
		return shell.QuotePOSIX(s)
	}
	selector := o.session
	if taskid.Valid(o.instance) {
		selector = o.instance
	}
	if o.local {
		identity := o.taskIdentity
		if identity == "" {
			identity = taskid.Key(o.instance)
		}
		if id := taskid.ParseKey(identity); id != "" {
			selector = id
		}
	}
	args := []string{"sshore", "attach", selector}
	if len(o.ssh) > 0 {
		args = append(args, "--ssh")
		args = append(args, o.ssh...)
	}
	for i := range args {
		args[i] = quote(args[i])
	}
	return strings.Join(args, " ")
}
