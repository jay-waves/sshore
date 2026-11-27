package tmux

import (
	"errors"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/term"
)

var ErrAttached = errors.New("session already attached")

// The application may embed Attachment to carry its control protocol state.
type attachmentOwner interface {
	comparable
	stream() *Attachment
}

// Task owns a native terminal and its active attachment. The daemon serializes
// operations so input ownership changes atomically with its registration and
// authorization decisions. Output, replay, and redraw use the same output lock.
type Task[A attachmentOwner, T any] struct {
	forest    *Forest[T]
	node      *treeNode[T]
	claim     *reservation[T]
	terminal  *pty.Terminal
	detached  time.Time
	active    A
	done      chan struct{}
	closing   bool
	modes     term.InputState
	reports   term.DirectoryReport
	pinUntil  *time.Time  // nil is unpinned, zero time is an indefinite pin.
	timer     *time.Timer // Startup expiry before Start, pin expiry after Start.
	scopeLost bool
}

func NewTask[A attachmentOwner, T any](forest *Forest[T], parent *Task[A, T]) Task[A, T] {
	task := Task[A, T]{forest: forest, node: new(treeNode[T]), done: make(chan struct{})}
	if parent != nil {
		forest.setParent(task.node, parent.node)
	}
	return task
}

func (t *Task[A, T]) SetIdentity(value T, label string) {
	t.node.Value, t.node.Label = value, label
}

func (t *Task[A, T]) Parent() (value T, ended bool) {
	if parent := t.node.Parent(); parent != nil {
		return parent.Value, parent.Ended()
	}
	return value, false
}

func (t *Task[A, T]) LoseScope() {
	if t.node.Parent() != nil {
		t.forest.setParent(t.node, nil)
	}
	t.scopeLost = true
}

func (t *Task[A, T]) Waiting() bool { return t.forest.waiting(t.node) }

func (t *Task[A, T]) Suspended(key string) bool { return t.forest.suspended(t.node, key) }

func (t *Task[A, T]) Hold(key string) (func(), bool) { return t.forest.hold(t.node, key) }

// ExpireUnstarted schedules startup cleanup while the caller holds the task
// lock. The callback acquires the same lock and rechecks whether Start won.
func (t *Task[A, T]) ExpireUnstarted(after time.Duration, expire func()) {
	if !t.Started() && !t.Exited() {
		t.setTimer(after, expire)
	}
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

func (t *Task[A, T]) Start(config pty.Config) error {
	terminal, err := pty.Start(config)
	if err == nil {
		t.stopTimer()
		t.terminal = terminal
	}
	return err
}

func (t *Task[A, T]) Started() bool         { return t.terminal != nil }
func (t *Task[A, T]) DetachedAt() time.Time { return t.detached }
func (t *Task[A, T]) Active() A             { return t.active }
func (t *Task[A, T]) Exited() bool          { return t.node.Ended() }

func (t *Task[A, T]) PID() int {
	if t.terminal == nil {
		return 0
	}
	return t.terminal.PID()
}

func (t *Task[A, T]) Done() <-chan struct{} { return t.done }

// BeginClose withdraws the terminal from the tree. Only its winner can
// publish completion, after notifying the frontend and closing its streams.
// The caller holds the task lock.
func (t *Task[A, T]) BeginClose() (owner A, ok bool) {
	if t.closing {
		return owner, false
	}
	t.closing = true
	return t.End(), true
}

// CompleteClose releases the native terminal before publishing completion.
func (t *Task[A, T]) CompleteClose() {
	if t.terminal != nil {
		_ = t.terminal.Close()
	}
	close(t.done)
}

func (t *Task[A, T]) Stop() error { return t.terminal.Stop() }

func (t *Task[A, T]) PumpOutput(consume func([]byte)) error {
	var buffer [8192]byte
	for {
		n, err := t.terminal.Read(buffer[:])
		if n > 0 {
			consume(buffer[:n])
		}
		if err != nil {
			return t.terminal.Wait()
		}
	}
}

// Attach reserves the tree, then publishes the input writer after readiness
// and replay succeed. Both operations are serialized by the caller's task lock.
func (t *Task[A, T]) Attach(owner A, key string, remoteParent bool, canAttach func() bool, prepare func(string) error) (parent T, err error) {
	adopted, err := t.forest.attach(t.node, key, remoteParent, canAttach, func(claim *reservation[T]) error {
		if t.claim != nil {
			return ErrAttached
		}
		if err := prepare(claim.key); err != nil {
			return err
		}
		owner.stream().input = &inputLease{terminal: t.terminal}
		t.active, t.claim = owner, claim
		return nil
	})
	if adopted != nil {
		t.scopeLost = false
		parent = adopted.Value
	}
	return parent, err
}

// ReleaseInput finishes in-flight writes before letting another owner attach.
// Even after the process exits, an old input pump still needs to be stopped.
func (t *Task[A, T]) ReleaseInput(owner A) (bool, error) {
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
		t.End()
		return false, err
	}
	t.forest.releaseInput(t.claim)
	var empty A
	t.active, t.claim = empty, nil
	t.detached = time.Now()
	return true, nil
}

func (t *Task[A, T]) End() A {
	old := t.active
	var empty A
	// Exit can precede the control loop's release. Revoke new writes now;
	// ReleaseInput will interrupt and drain any write already in flight.
	if old != empty {
		old.stream().input.stopped.Store(true)
	}
	t.active, t.claim = empty, nil
	t.stopTimer()
	t.forest.end(t.node)
	return old
}

func (t *Task[A, T]) Resize(size pty.Size) error { return t.terminal.Resize(size) }

func (t *Task[A, T]) ResizeAttached(owner A, size pty.Size) error {
	if t.active == owner {
		return t.Resize(size)
	}
	return nil
}

func (t *Task[A, T]) Redraw(owner A) {
	if t.active == owner {
		pty.RequestRedraw(t.terminal)
	}
}

func (t *Task[A, T]) Observe(data []byte, reportDirectory bool) string {
	if reportDirectory {
		return t.reports.Observe(&t.modes, data)
	}
	t.modes.Observe(data)
	return ""
}

func (t *Task[A, T]) Replay() []byte { return t.modes.Replay() }
