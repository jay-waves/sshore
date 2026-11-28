package tmux

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/term"
)

var ErrAttached = errors.New("session already attached")

const (
	StateStarting = "starting"
	StateActive   = "active"
	StateWaiting  = "waiting"
	StateDetached = "detached"
	StateExited   = "exited"
)

// The application may embed Attachment to carry its control protocol state.
type attachmentOwner interface {
	comparable
	stream() *Attachment
}

// Task owns a native terminal, its active attachment, and its lifecycle.
// The shared guard lets the application check authorization and task state
// together. Runtime transitions take it internally. The application holds it
// when reading state or preparing an authorized operation.
type Task[A attachmentOwner, T any] struct {
	guard                         *sync.Mutex
	outputMu                      sync.Mutex // Acquire before guard when ordering replay with live output.
	events                        Events[A, T]
	manager                       *Manager[A, T]
	name                          string
	node                          *treeNode[T]
	claim                         *reservation[T]
	terminal                      *pty.Terminal
	process                       atomic.Pointer[ProcessInfo]
	detached                      time.Time
	active                        A
	pendingTerminals              map[string]*terminalEndpoint
	done                          chan struct{}
	closing                       bool
	stopReason                    string
	modes                         term.InputState
	reports                       term.DirectoryReport
	directory                     string
	pinUntil                      *time.Time  // nil is unpinned, zero time is an indefinite pin.
	timer                         *time.Timer // Startup expiry before Start, pin expiry after Start.
	scopeLost                     bool
	ownerClaimed                  bool
	remoteOwner, remoteOwnerToken string
}

func (t *Task[A, T]) ownerIDLocked() string {
	if parent := t.node.Parent(); parent != nil && !parent.Ended() {
		return parent.id
	}
	return t.remoteOwner
}

func (t *Task[A, T]) Suspended(key string) bool { return t.manager.forest.suspended(t.node, key) }

func (t *Task[A, T]) Hold(key string) (func(), bool) {
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

func (t *Task[A, T]) setTimer(after time.Duration, run func()) {
	t.stopTimer()
	if after != 0 {
		t.timer = time.AfterFunc(after, run)
	}
}

func (t *Task[A, T]) stopTimer() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

type ProcessInfo struct{ Shell, ProgramLine string }

func (t *Task[A, T]) Process() ProcessInfo {
	if process := t.process.Load(); process != nil {
		return *process
	}
	return ProcessInfo{}
}

type Launch struct {
	Config          pty.Config
	Process         ProcessInfo
	ReportDirectory bool
}

func (t *Task[A, T]) startLocked(launch Launch) error {
	terminal, err := pty.Start(launch.Config)
	if err == nil {
		t.stopTimer()
		t.terminal = terminal
		t.process.Store(&launch.Process)
		go t.pumpOutput(launch.ReportDirectory)
	}
	return err
}

func (t *Task[A, T]) Started() bool     { return t.terminal != nil }
func (t *Task[A, T]) Active() A         { return t.active }
func (t *Task[A, T]) Exited() bool      { return t.node.Ended() }
func (t *Task[A, T]) Stopping() bool    { return t.stopReason != "" }
func (t *Task[A, T]) Directory() string { return t.directory }

func (t *Task[A, T]) stateLocked() string {
	var empty A
	switch {
	case t.Exited():
		return StateExited
	case t.active != empty:
		return StateActive
	case !t.Started():
		return StateStarting
	case t.manager.forest.waiting(t.node):
		return StateWaiting
	default:
		return StateDetached
	}
}

type Snapshot[A any] struct {
	ID, Name                            string
	OwnerID, ParentID, State, Directory string
	PID                                 int
	Process                             ProcessInfo
	Active                              A
	Pinned, Stopping, RemoteOwner       bool
	PinUntil, DetachedAt                time.Time
}

// Snapshot is a consistent view of a live session for publication or display.
func (t *Task[A, T]) Snapshot() Snapshot[A] {
	t.guard.Lock()
	defer t.guard.Unlock()
	return t.snapshotLocked()
}

func (t *Task[A, T]) snapshotLocked() Snapshot[A] {
	pinned, until := t.pinStateLocked()
	var parentID string
	if parent := t.node.Parent(); parent != nil {
		parentID = parent.id
	}
	return Snapshot[A]{ID: t.node.id, Name: t.name, OwnerID: t.ownerIDLocked(), ParentID: parentID, State: t.stateLocked(), Directory: t.directory,
		PID: t.PID(), Process: t.Process(), Active: t.active, Pinned: pinned, PinUntil: until,
		DetachedAt: t.detached, Stopping: t.Stopping(), RemoteOwner: t.remoteOwner != ""}
}

func (t *Task[A, T]) PID() int {
	if t.terminal == nil {
		return 0
	}
	return t.terminal.PID()
}

func (t *Task[A, T]) Done() <-chan struct{} { return t.done }

func (t *Task[A, T]) pumpOutput(reportDirectory bool) {
	var buffer [8192]byte
	for {
		n, err := t.terminal.Read(buffer[:])
		if n > 0 {
			t.forwardOutput(buffer[:n], reportDirectory)
		}
		if err != nil {
			t.terminalExited(t.terminal.Wait())
			return
		}
	}
}

func (t *Task[A, T]) forwardOutput(data []byte, reportDirectory bool) {
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
	var empty A
	if owner != empty {
		_ = owner.stream().Output(data)
	}
	t.outputMu.Unlock()
	if changed {
		t.changed()
	}
}

// AttachRequest carries the requested terminal settings and a readiness reply.
// Ready receives the session state before replay or live bytes reach the frontend.
type AttachRequest struct {
	TreeKey, OwnerToken, TerminalToken            string
	Size                                          pty.Size
	Existing, RetryUnstarted, Pin, StartupOptions bool
	Launch                                        Launch
	LaunchError, ValidationError                  error
}

func (t *Task[A, T]) attach(owner A, req AttachRequest, ready func(string, Snapshot[A]) error) error {
	t.outputMu.Lock()
	t.guard.Lock()
	initial := !t.Started()
	if err := t.checkOwnerLocked(req.OwnerToken); err != nil {
		t.guard.Unlock()
		t.outputMu.Unlock()
		return err
	}
	if t.Stopping() || initial && req.Existing {
		t.guard.Unlock()
		t.outputMu.Unlock()
		return errors.New("shell changed or is unavailable; refresh with sshore list")
	}
	prepared, err := t.attachLocked(owner, req.TreeKey, t.remoteOwner != "", func(key string) error {
		if initial {
			if req.LaunchError != nil {
				return req.LaunchError
			}
			req.Launch.Config.Size = req.Size
			if err := t.startLocked(req.Launch); err != nil {
				return err
			}
			if req.Pin {
				if err := t.pinLocked(0); err != nil {
					return err
				}
			}
		} else {
			if req.StartupOptions {
				return errors.New("startup options require a new managed shell")
			}
			if err := t.terminal.Resize(req.Size); err != nil {
				return err
			}
		}
		return ready(key, t.snapshotLocked())
	})
	if err == nil && !initial {
		pty.RequestRedraw(t.terminal)
	}
	abort := err != nil && initial && prepared && !errors.Is(err, ErrAttached) && !errors.Is(err, ErrEnded) &&
		(!req.RetryUnstarted || t.Started())
	if abort {
		t.stopReason = "initial attachment failed"
		t.endLocked()
	}
	t.guard.Unlock()
	t.outputMu.Unlock()
	t.changed()
	if abort {
		if req.RetryUnstarted {
			t.Close()
		} else {
			_ = t.stopTerminal()
		}
	}
	return err
}

func (t *Task[A, T]) attachLocked(owner A, key string, remoteParent bool, prepare func(string) error) (prepared bool, err error) {
	adopted, err := t.manager.forest.attach(t.node, key, remoteParent, func() bool {
		t.reapUnretainedLocked()
		return !t.Exited() && !t.Stopping()
	}, func(claim *reservation[T]) error {
		if t.claim != nil {
			return ErrAttached
		}
		prepared = true
		if err := prepare(claim.key); err != nil {
			return err
		}
		attachment := owner.stream()
		if data := t.modes.Replay(); len(data) > 0 {
			if err := attachment.Output(data); err != nil {
				return err
			}
		}
		attachment.input = &inputLease{terminal: t.terminal}
		t.active, t.claim = owner, claim
		return nil
	})
	if adopted != nil {
		t.scopeLost = false
		t.watchParent(adopted)
	}
	return prepared, err
}

type Detachment struct {
	Released bool
	Pinned   bool
	PinUntil time.Time
}

type PinFailure struct{ error }

// Detach cancels the old input writer before publishing the new state.
// A non-nil retention duration pins the session as part of that same change.
func (t *Task[A, T]) Detach(owner A, retention *time.Duration) (Detachment, error) {
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
func (t *Task[A, T]) releaseInputLocked(owner A) (bool, error) {
	a := owner.stream()
	active := t.active == owner
	if a.input == nil || !active && !t.Exited() {
		return false, nil
	}
	input := a.input
	err := input.Stop()
	defer func() { a.input = nil }()
	if !active {
		return false, nil
	}
	if err != nil {
		t.stopReason = "terminal input cancellation failed"
		t.endLocked()
		return false, err
	}
	t.manager.forest.releaseInput(t.claim)
	var empty A
	t.active, t.claim = empty, nil
	t.detached = time.Now()
	return true, nil
}

func (t *Task[A, T]) endLocked() A {
	old := t.active
	var empty A
	// Exit can precede the control loop's release. Revoke new writes now;
	// Detach will interrupt and drain any write already in flight.
	if old != empty {
		old.stream().input.stopped.Store(true)
	}
	t.active, t.claim = empty, nil
	t.stopTimer()
	t.manager.forest.end(t.node)
	return old
}

func (t *Task[A, T]) ResizeAttached(owner A, size pty.Size) error {
	t.guard.Lock()
	defer t.guard.Unlock()
	if t.active == owner {
		return t.terminal.Resize(size)
	}
	return nil
}

func (t *Task[A, T]) Redraw(owner A) {
	t.outputMu.Lock()
	defer t.outputMu.Unlock()
	t.guard.Lock()
	defer t.guard.Unlock()
	if t.active == owner {
		pty.RequestRedraw(t.terminal)
	}
}
