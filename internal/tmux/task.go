package tmux

import (
	"cmp"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/term"
)

var (
	ErrAttached    = errors.New("session already attached")
	ErrUnavailable = errors.New("session changed or is unavailable")
)

const (
	StateStarting = "starting"
	StateActive   = "active"
	StateWaiting  = "waiting"
	StateDetached = "detached"
	StateExited   = "exited"
)

// Session owns a process, its terminal and the active attachment.
type Session struct {
	guard        sync.Mutex
	outputMu     sync.Mutex // Orders terminal output and replay.
	pending      *Attachment
	events       Events
	manager      *Manager
	name         string
	node         *treeNode
	claim        *reservation
	terminal     *pty.Terminal
	process      ProcessInfo
	config       ProcessConfig
	retryStart   bool
	detached     time.Time
	active       *Attachment
	done         chan struct{}
	closing      bool
	stopReason   string
	modes        term.InputState
	reports      term.DirectoryReport
	directory    string
	pinUntil     *time.Time  // nil is unpinned, zero time is an indefinite pin.
	timer        *time.Timer // Startup expiry before Start, pin expiry after Start.
	scopeLost    bool
	ownerClaimed bool
}

func (t *Session) ownerIDLocked() string {
	if parent := t.node.Parent(); parent != nil && !parent.Ended() {
		return parent.id
	}
	return ""
}

func (t *Session) Hold(key string) (func(), bool) {
	release, ok := t.manager.forest.hold(t.node, key)
	if !ok {
		return nil, false
	}
	t.changed()
	return func() {
		release()
		t.changed()
	}, true
}

func (t *Session) setTimer(after time.Duration, run func()) {
	t.stopTimer()
	if after != 0 {
		t.timer = time.AfterFunc(after, run)
	}
}

func (t *Session) stopTimer() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

type ProcessInfo struct{ Shell, ProgramLine string }

func (t *Session) startedLocked() bool  { return t.terminal != nil }
func (t *Session) exited() bool         { return t.node.Ended() }
func (t *Session) stoppingLocked() bool { return t.stopReason != "" }

func (t *Session) stateLocked() string {
	switch {
	case t.exited():
		return StateExited
	case t.active != nil:
		return StateActive
	case !t.startedLocked():
		return StateStarting
	case t.pending != nil || t.manager.forest.waiting(t.node):
		return StateWaiting
	default:
		return StateDetached
	}
}

type Snapshot struct {
	ID, Name                            string
	OwnerID, ParentID, State, Directory string
	PID                                 int
	Process                             ProcessInfo
	Active                              *Attachment
	Pinned, Stopping                    bool
	Attaching                           bool
	PinUntil, DetachedAt                time.Time
}

// Snapshot is a consistent view of a live session for publication or display.
func (t *Session) Snapshot() Snapshot {
	t.guard.Lock()
	defer t.guard.Unlock()
	return t.snapshotLocked()
}

func (t *Session) snapshotLocked() Snapshot {
	pinned, until := t.pinStateLocked()
	var parentID string
	if parent := t.node.Parent(); parent != nil {
		parentID = parent.id
	}
	return Snapshot{ID: t.node.id, Name: t.name, OwnerID: t.ownerIDLocked(), ParentID: parentID, State: t.stateLocked(), Directory: cmp.Or(t.directory, t.config.Directory),
		PID: t.pidLocked(), Process: t.process, Active: t.active, Pinned: pinned, PinUntil: until,
		DetachedAt: t.detached, Attaching: t.pending != nil, Stopping: t.stoppingLocked()}
}

func (t *Session) pidLocked() int {
	if t.terminal == nil {
		return 0
	}
	return t.terminal.PID()
}

// WorkingDirectory prefers the terminal's report, then the live process's
// directory and finally the configured startup directory.
func (t *Session) WorkingDirectory() string {
	t.guard.Lock()
	defer t.guard.Unlock()
	if t.directory != "" {
		return t.directory
	}
	if pid := t.pidLocked(); pid > 0 && !t.exited() {
		if directory, err := sys.WorkingDirectory(pid); err == nil && directory != "" {
			return directory
		}
	}
	return t.config.Directory
}

func (t *Session) Done() <-chan struct{} { return t.done }

func (t *Session) pumpOutput(reportDirectory bool) {
	var buffer [8192]byte
	for {
		n, err := t.terminal.Read(buffer[:])
		if n > 0 {
			t.forwardOutput(buffer[:n], reportDirectory)
		}
		if err != nil {
			exit := t.terminal.Wait()
			// A program that exits immediately still gets its ready reply first.
			t.outputMu.Lock()
			t.outputMu.Unlock()
			t.terminalExited(exit)
			return
		}
	}
}

func (t *Session) forwardOutput(data []byte, reportDirectory bool) {
	t.outputMu.Lock()
	var directory string
	if reportDirectory {
		directory = t.reports.Observe(&t.modes, data)
	} else {
		t.modes.Observe(data)
	}
	t.guard.Lock()
	changed := directory != "" && directory != t.directory
	if changed {
		t.directory = directory
	}
	owner := t.active
	t.guard.Unlock()
	if owner != nil {
		_ = owner.Output(data)
	}
	t.outputMu.Unlock()
	if changed {
		t.changed()
	}
}

// AttachOptions supplies the requested terminal size and first-start options.
type AttachOptions struct {
	TreeKey, Term string
	Size          pty.Size
	Existing, Pin bool
	Startup       Startup
}

// Attach reserves the foreground, sends readiness, and starts exchanging bytes.
// The reservation survives while ready and replay write without the session lock.
// Stopping closes owner.Done(), which the caller can use to cancel ready.
// The caller closes the channel if Attach fails.
func (t *Session) Attach(owner *Attachment, req AttachOptions, ready func(string, Snapshot) error) error {
	t.guard.Lock()
	if err := t.checkAttachLocked(req); err != nil {
		t.guard.Unlock()
		return err
	}
	if t.pending != nil || t.active != nil {
		t.guard.Unlock()
		return ErrAttached
	}
	forest := &t.manager.forest
	r, err := forest.reserve(t.node, req.TreeKey, func() bool {
		t.reapUnretainedLocked()
		return !t.stoppingLocked()
	})
	if err != nil {
		t.guard.Unlock()
		return err
	}
	t.pending = owner
	t.guard.Unlock()

	t.outputMu.Lock()
	state, initial, err := t.prepareAttachment(req)
	if err == nil && ready != nil {
		err = ready(r.claim.key, state)
	}
	if err == nil {
		if data := t.modes.Replay(); len(data) > 0 {
			err = owner.Output(data)
		}
	}

	t.guard.Lock()
	if err == nil {
		err = t.checkAttachLocked(req)
	}
	if err == nil {
		select {
		case <-owner.Done():
			err = io.ErrClosedPipe
		default:
		}
	}
	if !forest.finishAttach(r, err == nil) && err == nil {
		err = ErrUnavailable
	}
	t.pending = nil
	if err == nil {
		owner.input = &inputLease{terminal: t.terminal}
		t.active, t.claim = owner, r.claim
		if r.adopted != nil {
			t.scopeLost = false
			t.watchParent(r.adopted)
		}
		t.startInput(owner)
		if !initial {
			pty.RequestRedraw(t.terminal)
		}
	}
	abort := err != nil && initial && !t.exited() && !t.stoppingLocked() &&
		(!t.retryStart || t.startedLocked())
	if abort {
		t.stopReason = "initial attachment failed"
		t.endLocked()
	}
	t.guard.Unlock()
	t.outputMu.Unlock()
	t.changed()
	if abort {
		if t.retryStart {
			t.Close()
		} else {
			_ = t.stopTerminal()
		}
	}
	return err
}

func (t *Session) checkAttachLocked(req AttachOptions) error {
	if t.exited() {
		return ErrEnded
	}
	if t.stoppingLocked() || req.Existing && !t.startedLocked() {
		return ErrUnavailable
	}
	return nil
}

func (t *Session) prepareAttachment(req AttachOptions) (Snapshot, bool, error) {
	t.guard.Lock()
	defer t.guard.Unlock()
	if err := t.checkAttachLocked(req); err != nil {
		return Snapshot{}, false, err
	}
	initial := !t.startedLocked()
	if initial {
		if err := t.startLocked(req.Startup, req.Size, req.Term); err != nil {
			return Snapshot{}, initial, err
		}
		if req.Pin {
			if err := t.pinLocked(0); err != nil {
				return Snapshot{}, initial, err
			}
		}
	} else {
		if req.Startup.configured() {
			return Snapshot{}, initial, errors.New("startup options require a new managed shell")
		}
		if err := t.terminal.Resize(req.Size); err != nil {
			return Snapshot{}, initial, err
		}
	}
	return t.snapshotLocked(), initial, nil
}

type Detachment struct {
	Released bool
	Pinned   bool
	PinUntil time.Time
}

type PinFailure struct{ error }

// Detach cancels the old input writer before publishing the new state.
// A non-nil retention duration pins the session as part of that same change.
func (t *Session) Detach(owner *Attachment, retention *time.Duration) (Detachment, error) {
	t.guard.Lock()
	if retention != nil {
		if t.active != owner {
			t.guard.Unlock()
			return Detachment{}, PinFailure{errors.New("shell has no active attachment")}
		}
		if err := t.pinLocked(*retention); err != nil {
			t.guard.Unlock()
			return Detachment{}, PinFailure{err}
		}
	}
	released, err := t.releaseInputLocked(owner)
	if released {
		t.reapUnretainedLocked()
	}
	pinned, until := t.pinStateLocked()
	t.guard.Unlock()
	if released || err != nil {
		t.changed()
	}
	if err != nil {
		t.Close()
	}
	return Detachment{Released: released, Pinned: pinned, PinUntil: until}, err
}

// Even after the process exits, an old input pump still needs to be stopped.
func (t *Session) releaseInputLocked(owner *Attachment) (bool, error) {
	active := t.active == owner
	if owner.input == nil || !active && !t.exited() {
		return false, nil
	}
	input := owner.input
	err := input.Stop()
	defer func() { owner.input = nil }()
	if !active {
		return false, nil
	}
	if err != nil {
		t.stopReason = "terminal input cancellation failed"
		t.endLocked()
		return false, err
	}
	t.manager.forest.releaseInput(t.claim)
	t.active, t.claim = nil, nil
	t.detached = time.Now()
	return true, nil
}

func (t *Session) endLocked() *Attachment {
	old := t.active
	// Exit can precede the control loop's release. Revoke new writes now;
	// Detach will interrupt and drain any write already in flight.
	if old != nil {
		old.input.stopped.Store(true)
	}
	t.active, t.claim = nil, nil
	t.stopTimer()
	t.manager.forest.end(t.node)
	return old
}

func (t *Session) ResizeAttached(owner *Attachment, size pty.Size) error {
	t.guard.Lock()
	defer t.guard.Unlock()
	if t.active == owner {
		return t.terminal.Resize(size)
	}
	return nil
}

func (t *Session) Redraw(owner *Attachment) {
	t.guard.Lock()
	defer t.guard.Unlock()
	if t.active == owner {
		pty.RequestRedraw(t.terminal)
	}
}

// Inspect keeps a session state check and a short in-memory operation atomic.
// The operation must not block on I/O or call the session.
func (t *Session) Inspect(operation func(Snapshot) error) error {
	t.guard.Lock()
	defer t.guard.Unlock()
	return operation(t.snapshotLocked())
}
