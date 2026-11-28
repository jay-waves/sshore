package files

import (
	"container/list"
	"context"
	"errors"
	"io/fs"
	"sync"
	"time"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

const (
	mountMetadata = iota
	mountData
)

const mountInteractiveReadSize = 4 * 1024

// Leave space for metadata even when bulk transfers have filled their window.
// The combined limit stays below the SFTP client's 64-slot result pool.
var mountRequestWindows = [2]int{8, 16}

var errMountRequestQueueTimeout = errors.New("SFTP request timed out waiting for a send slot")

func newMountSFTPClient(ctx context.Context, wire *ftpWire, timeout time.Duration) (*sftp.Client, error) {
	scheduler := &mountRequestScheduler{ctx: ctx, timeout: timeout}
	return sftp.NewClientPipe(ctx, wire, wire, sftp.WithMaxDataLength(mountSFTPDataLength),
		sftp.WithDispatchAdmission(scheduler.admit))
}

type mountRequestScheduler struct {
	ctx     context.Context
	timeout time.Duration

	mu      sync.Mutex
	queues  [2]list.List
	active  [2]int
	next    int
	sending bool
}

func (s *mountRequestScheduler) admit(disconnected, cancelled <-chan struct{}, packet sshfx.PacketMarshaller) (sent, release func(), err error) {
	class := mountMetadata
	switch packet := packet.(type) {
	case *sshfx.ReadPacket:
		if packet.Length > mountInteractiveReadSize {
			class = mountData
		}
	case *sshfx.WritePacket:
		class = mountData
	}
	ready := make(chan struct{})
	s.mu.Lock()
	entry := s.queues[class].PushBack(ready)
	s.grant()
	s.mu.Unlock()

	var deadline <-chan time.Time
	if s.timeout > 0 {
		timer := time.NewTimer(s.timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-s.ctx.Done():
		err = s.ctx.Err()
	case <-disconnected:
		err = sshfx.StatusConnectionLost
	case <-cancelled:
		err = fs.ErrClosed
	case <-deadline:
		err = errMountRequestQueueTimeout
	case <-ready:
		return func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.sending = false
				s.grant()
			}, func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.active[class]--
				s.grant()
			}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-ready:
		// Cancellation raced a grant, so this request owns both slots.
		s.sending = false
		s.active[class]--
	default:
		s.queues[class].Remove(entry)
	}
	s.grant()
	return nil, nil, err
}

// grant selects a single packet writer. Alternate between eligible queues
// and preserve arrival order within each queue.
func (s *mountRequestScheduler) grant() {
	if s.sending {
		return
	}
	for _, class := range [2]int{s.next, 1 - s.next} {
		queue := &s.queues[class]
		if queue.Len() == 0 || s.active[class] == mountRequestWindows[class] {
			continue
		}
		s.active[class]++
		s.next = 1 - class
		s.sending = true
		close(queue.Remove(queue.Front()).(chan struct{}))
		return
	}
}
