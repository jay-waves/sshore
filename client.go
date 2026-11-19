package main

// The frontend owns terminal interaction; daemon-owned tasks outlive its
// connections. Remote operation requests are stored for separate local review.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/xtaci/smux"
	"golang.org/x/term"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/tmux"
	"github.com/jay-waves/sshore/internal/wire"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

type clientEvent struct {
	keyAction      tmux.KeyAction
	inputFailure   bool
	approvalStream *smux.Stream
	approval       []operation
	m              message
	input          []byte
	err            error
}

// Learn the current shell name before saving approval metadata.
func acceptSessionName(o *options, name string) error {
	if o.session != "" && !(o.command == "attach" && (!o.local || o.hosted) && validTaskUID(o.instance)) {
		return nil
	}
	if err := validSession(name); err != nil {
		return fmt.Errorf("server returned an invalid shell name: %w", err)
	}
	o.session = name
	return nil
}

func runClientOnce(o options, previous string, transports clientTransports, ready func(options)) (resultCode int, resultErr error) {
	attached := false
	var ended func()
	// Report after input shutdown and console restoration, including nested returns.
	defer func() {
		if ended != nil {
			ended()
			return
		}
		if _, switching := errors.AsType[*sessionSwitch](resultErr); attached && resultErr != nil && !switching {
			sessionNotice(termui.Warning, "Disconnected from %s on %s (client code %d; %s). Shell state unconfirmed.", shellTaskName(cleanText(o.session), o.temporary), cleanText(o.host), resultCode, sessionDisconnectReason(resultErr))
		}
	}()
	signalCtx, interruptSignals, stopSignals := newClientSignals(o.recovering)
	defer stopSignals()
	// Cancel I/O before restoring the console, but keep signal handling installed
	// until the keyboard has stopped and terminal restoration has completed.
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	killedInstance := o.instance
	if o.command == "kill" {
		if cleared, err := forgetEndedShell(o.host, o.session, o.instance); cleared || err != nil {
			return 0, err
		}
		snapshots, err := readShellSnapshots()
		if err != nil {
			return 1, err
		}
		for _, s := range snapshots {
			if s.Host == o.host && s.Name == o.session && (o.instance == "" || s.Instance == o.instance) {
				killedInstance = s.Instance
				break
			}
		}
	}
	if o.list || o.command == "kill" || o.command == "kill-resource" || o.command == "rename" {
		err := transportio.Run(context.Background(), sshConnectionOptions(o), os.Stdin, os.Stdout, os.Stderr)
		if err == nil && o.command == "kill" {
			if cacheErr := confirmKilledShell(o.host, o.session, killedInstance); cacheErr != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Cancelled; local history not updated: %s\n", cleanText(cacheErr.Error()))
			}
		}
		if err == nil && o.command == "rename" {
			if cacheErr := rememberShellRename(o.host, o.session, o.newName, o.instance); cacheErr != nil {
				termui.Log(os.Stderr, termui.Warning, "[sshore] Renamed; local history not updated: %s\n", cleanText(cacheErr.Error()))
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
	tmux.ResetInputModes()
	transport, err := openClientTransportContext(ctx, &o, transports)
	if err != nil {
		return 1, err
	}
	defer transport.Close()
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
	tree := o.tree
	if tree == nil {
		tree = currentShellTree()
	}
	sourceMachine, sourceHost := localMachineIdentity()
	if err = streamMessage(control, message{Type: "attach", OwnerProcess: describeSpawnOwner(o.owner), SourceHost: sourceHost, SourceMachineID: sourceMachine, Tree: tree, Instance: o.instance, Action: action, Cols: uint16(cols), Rows: uint16(rows), Term: termName, NoProfile: o.noProfile && (o.command == "launch" || o.command == "spawn"), NoLogin: o.noLogin && (o.command == "launch" || o.command == "spawn")}); err != nil {
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
	if (!o.local || o.hosted) && validTaskUID(o.instance) && o.instance != m.Instance {
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
	connection, cacheErr := trackShellConnection(o)
	if cacheErr != nil {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Shell tracking failed: %s\n", cleanText(cacheErr.Error()))
	}
	defer connection.close()
	o.shellPID = m.PID
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

	confirmDetach := func(name string) {
		connection.confirm(name, "detached")
		if validSession(name) == nil {
			o.session = name
		}
		if err := rememberDetachedAttachment(o); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Detach history not saved: %s\n", cleanText(err.Error()))
		}
	}
	if o.detached {
		if err := tmux.Detach(control, confirmDetach); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stdout, "Started %s (%s).\n", cleanText(shellDisplayEndpoint(o)), shellTaskKind(o.temporary))
		return 0, nil
	}
	if !o.local && !o.quiet {
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
	if !o.local {
		if e := rememberConnection(o); e != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Connection history not saved: %s\n", cleanText(e.Error()))
		}
	}

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
	// One terminal writer preserves keystroke order without making file I/O or a
	// stalled PTY block the main UI/control loop.
	inputQueue := make(chan []tmux.KeyEvent, 16)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
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
	defer func() {
		cancel()
		stopKeyboard()
		_ = transport.Close()
		<-inputDone
	}()
	if o.handoffDone != nil {
		if err := o.handoffDone(ctx, nil); err != nil {
			return 1, err
		}
		o.handoffDone = nil
	}
	o.recovering = false
	interruptSignals.interactive.Store(true)
	attached = true
	ready(o)

	sessionNotice(termui.Success, "Connected %s (%s)", cleanText(shellDisplayEndpoint(o)), shellTaskKind(o.temporary))
	color := termui.ColorEnabled(os.Stdout)
	notice := func(level termui.Color, format string, args ...any) {
		fmt.Fprint(os.Stdout, termui.Format(color, level, format, args...))
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var outputMouse tmux.InputState
	keys := tmux.Keys{Bypass: o.noEscape}
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
		case inputQueue <- []tmux.KeyEvent{{Action: tmux.KeyDetach}}:
			drainInput()
			return true
		default:
			return false
		}
	}
	sendDetach := func(inputUnconfirmed bool) error {
		if inputUnconfirmed {
			notice(termui.Warning, "\r\n[sshore] Input delivery unconfirmed; detaching.\r\n")
		}
		if err := streamMessage(control, message{Type: "detach"}); err != nil {
			return err
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
	clearTransition := func() { pending = transitionRequest{} }
	awaitResponse := func(delay time.Duration) {
		pending.deadline = time.Now().Add(delay)
		pending.timeout = time.After(time.Until(pending.deadline))
	}
	transitioning := func() bool { return pending.action != "" || pending.next != nil }
	actionForKey := func(action tmux.KeyAction) string {
		switch action {
		case tmux.KeyNew:
			return "spawn_child"
		case tmux.KeyNext:
			return "next"
		case tmux.KeyPrevious:
			return "previous"
		case tmux.KeyLast:
			return "last"
		}
		return ""
	}
	queueSwitch := func(next *sessionSwitch) bool {
		if !queueDetach() {
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
			go func() { emit(clientEvent{m: localSessionTargets(current, request)}) }()
			return nil
		}
		return streamMessage(control, request)
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
		case inputQueue <- []tmux.KeyEvent{{Data: []byte{3}}}:
			return nil
		default:
			return fmt.Errorf("terminal input queue is busy")
		}
	}
	for {
		select {
		case <-interruptSignals.interrupts:
			if err := interrupt(); err != nil {
				return 1, err
			}
		case <-ctx.Done():
			return 130, ctx.Err()
		case <-keySequenceTimeout:
			keySequenceTimeout = nil
			batch := keys.FlushPending()
			if len(batch) > 0 && !transitioning() && detachPhase == detachIdle {
				select {
				case inputQueue <- batch:
				default:
					return 255, fmt.Errorf("terminal input stalled; closing connection; reconnect to check the shell")
				}
			}
		case <-pending.timeout:
			if pending.action == "handoff" {
				report := pending.next.to.handoffDone
				go func() { _ = report(ctx, context.DeadlineExceeded) }()
				notice(termui.Warning, "\r\n[sshore] Handoff timed out; staying here.\r\n")
			} else {
				notice(termui.Warning, "\r\n[sshore] Session lookup timed out; staying here.\r\n")
			}
			clearTransition()
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
			if e.keyAction != tmux.KeyNone {
				var keyErr error
				if detachPhase != awaitingDetach {
					switch e.keyAction {
					case tmux.KeyNew, tmux.KeyNext, tmux.KeyPrevious, tmux.KeyLast:
						action := actionForKey(e.keyAction)
						if detachPhase != detachIdle || action != pending.action {
							break
						}
						if e.keyAction == tmux.KeyNew {
							awaitResponse(30 * time.Second)
							keyErr = streamMessage(control, message{Type: action})
						} else {
							keyErr = requestTargets(action)
						}
					case tmux.KeyDetach:
						keyErr = sendDetach(false)
					case tmux.KeyRedraw:
						keyErr = streamMessage(control, message{Type: "redraw"})
					}
				}
				keyHandled <- struct{}{}
				if keyErr != nil {
					return 255, keyErr
				}
				continue
			}
			if e.approvalStream != nil {
				st := e.approvalStream
				if detachPhase != detachIdle || transitioning() {
					st.Close()
					continue
				}
				request, err := queueRequest(o, e.approval)
				if err != nil {
					_ = streamMessage(st, message{Type: "error", Error: err.Error()})
					st.Close()
					notice(termui.Error, "\r\n[sshore] Request rejected: %s\r\n", cleanText(err.Error()))
					continue
				}
				if err := confirmQueuedRequest(st, request); err != nil {
					return 1, err
				}
				notice(termui.Info, "\r\n[sshore] Request saved: %s\r\n[sshore] Run sshore review from your LOCAL shell after this client exits.\r\n", request.ID)
				if err = sendDetach(false); err != nil {
					return 1, err
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
			} else if e.input != nil {
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
				if detachPhase != detachIdle {
					continue
				}
				batch := keys.Feed(e.input)
				armKeyTimeout()
				if len(batch) > 0 {
					select {
					case inputQueue <- batch:
						for _, key := range batch {
							if action := actionForKey(key.Action); action != "" {
								pending.action = action
							}
							if key.Action == tmux.KeyDetach {
								drainInput()
								break
							}
						}
					default:
						return 255, fmt.Errorf("terminal input stalled; closing connection; reconnect to check the shell")
					}
				}

			} else {
				switch e.m.Type {
				case "spawn_child_error":
					if pending.timeout != nil && pending.action == "spawn_child" {
						clearTransition()
					}
					notice(termui.Warning, "\r\n[sshore] %s\r\n", cleanText(e.m.Error))
				case "session_targets":
					if pending.timeout == nil || pending.action == "handoff" || e.m.Action != pending.action {
						continue
					}
					clearTransition()
					if e.m.Error != "" {
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

				case "output":
					if detachPhase == awaitingDetach {
						continue
					}
					if err = wire.WriteFull(terminalOutput, e.m.Data); err != nil {
						return 1, err
					}
					outputMouse.Observe(e.m.Data)
					if err = tmux.SyncMouse(&outputMouse); err != nil {
						return 1, fmt.Errorf("update terminal mouse mode: %w", err)
					}
				case "spawn_route":
					if pending.timeout != nil && pending.action == "spawn_child" {
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
					if detachPhase != detachIdle || transitioning() {
						reject()
						continue
					}
					target := options{noLogin: e.m.NoLogin, noProfile: e.m.NoProfile, tree: e.m.Tree, command: "spawn", host: e.m.SourceHost, session: e.m.Name, temporary: true, routeParent: &parent, routeToken: e.m.Token, quiet: o.quiet}
					if e.m.Action == "local" {
						target.host = o.host
					}
					target.handoffDone = func(ctx context.Context, result error) error { return confirmSpawnRoute(ctx, parent, token, result) }
					if !queueSwitch(&sessionSwitch{to: target, returnToParent: true}) {
						reject()
					}
				case "handoff":
					if pending.timeout != nil && pending.action == "spawn_child" {
						clearTransition()
					}
					// Only authenticated local control can redirect this frontend.
					if !o.local {
						return 1, fmt.Errorf("remote attachment requested local handoff")
					}
					target, handoffErr := e.m.Handoff.options()
					source, token := o, e.m.Token
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
					if detachPhase != detachIdle || transitioning() {
						handoffErr = fmt.Errorf("client is switching sessions")
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
						go func() { _ = target.to.handoffDone(ctx, handoffErr) }()
						notice(termui.Warning, "\r\n[sshore] %s.\r\n", handoffErr)
					}
				case "detach_requested":
					if detachPhase == detachIdle {
						if err = sendDetach(false); err != nil {
							return 255, err
						}
					}
				case "detached":
					confirmDetach(e.m.Name)
					_ = streamMessage(control, message{Type: "bye"})
					if pending.action == "" && pending.next != nil {
						pending.next.from = returnTarget{options: o, previous: previous}
						return 0, pending.next
					}
					if o.temporary {
						sessionNotice(termui.Success, "Background; %s on %s ends with the original parent shell.\r\nReturn: %s", shellTaskName(cleanText(o.session), true), cleanText(o.host), reconnectCommand(o))
					} else {
						sessionNotice(termui.Success, "Detached; %s on %s keeps running.\r\nReconnect: %s", shellTaskName(cleanText(o.session), false), cleanText(o.host), reconnectCommand(o))
					}
					return 0, nil
				case "exit":
					connection.confirm(e.m.Name, "exited")
					level := termui.Success
					if e.m.Code != 0 {
						level = termui.Warning
					}
					exit := e.m
					ended = func() {
						reason := cleanText(exit.Reason)
						if reason == "" || strings.HasPrefix(reason, "shell exited (code ") {
							reason = "shell exited"
						}
						sessionNotice(level, "Exited %s on %s (code %d). Reason: %s.", shellTaskName(cleanText(o.session), o.temporary), cleanText(o.host), exit.Code, reason)
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

func sessionNotice(level termui.Color, format string, args ...any) {
	termui.Log(os.Stderr, level, "\r\n\r\n\r\n[sshore] "+format+"\r\n\r\n", args...)
}

// Retain transport options in the reconnect hint, including custom ports and keys.
// Use POSIX shell quoting on Linux and PowerShell quoting on Windows.
func shellDisplayEndpoint(o options) string {
	if o.local {
		return localEndpointPrefix + o.session
	}
	return remotePathOperand(o.host, o.session)
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
	args := []string{"sshore", "attach", remotePathOperand(o.host, selector)}
	if o.local {
		identity := o.taskIdentity
		if identity == "" {
			identity = remoteShellIdentity(o.host, o.instance)
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
