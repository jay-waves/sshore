package main

// Each outstanding SFTP request has its own deadline. A responsive sibling or
// SSH keepalive cannot hide one stuck request. Timeout tears down the transport;
// writes are never replayed and their remote outcome can be unknown. Idle mounts
// have no request deadline. Authentication is covered only by an opt-in startup
// deadline, since the subsystem protocol cannot distinguish an SSH prompt.
import (
	"fmt"
	"sync"
	"time"
)

type sftpWatch struct {
	mu      sync.Mutex
	failure error
	timer   *time.Timer
}

func (w *sftpWatch) fail(err error, abort func()) {
	w.mu.Lock()
	first := w.failure == nil
	if first {
		w.failure = err
	}
	w.mu.Unlock()
	if first {
		abort()
	}
}
func (w *sftpWatch) err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.failure }
func (w *ftpWire) watchRequests(timeout time.Duration, watch *sftpWatch, abort func()) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if timeout <= 0 {
			<-stop
			return
		}
		tick := time.NewTicker(min(time.Second, max(time.Millisecond, timeout/4)))
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				w.mu.Lock()
				expired := false
				for _, r := range w.requests {
					if !r.started.IsZero() && now.Sub(r.started) >= timeout {
						expired = true
						break
					}
				}
				w.mu.Unlock()
				if expired {
					watch.fail(fmt.Errorf("SFTP request timed out after %s; connection closed, remote writes may be incomplete or have completed; no retry performed", timeout), abort)
					return
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}
