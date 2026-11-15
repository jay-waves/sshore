package mux

import (
	"errors"
	"sync"
)

var ErrExited = errors.New("session is exiting")
var ErrAttached = errors.New("session already attached")
var ErrMissingAttachment = errors.New("missing attachment")

// Lifecycle owns exclusive attachments. A is an attachment identity, normally a
// pointer. Its zero value is detached. Prepare must not call back into Lifecycle.
type Lifecycle[A comparable] struct {
	mu     sync.Mutex
	active A
	exited bool
}

func (s *Lifecycle[A]) Active() A    { s.mu.Lock(); defer s.mu.Unlock(); return s.active }
func (s *Lifecycle[A]) Exited() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.exited }
func (s *Lifecycle[A]) Attach(a A, prepare func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero A
	if a == zero {
		return ErrMissingAttachment
	}
	if s.exited {
		return ErrExited
	}
	if s.active != zero {
		return ErrAttached
	}
	if err := prepare(); err != nil {
		return err
	}
	s.active = a
	return nil
}

// Detach preserves the terminal; stale identities cannot detach another client.
func (s *Lifecycle[A]) Detach(a A) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != a {
		return false
	}
	var zero A
	if a == zero {
		return false
	}
	s.active = zero
	return true
}

// Release treats transport loss as terminal termination only for the owner.
func (s *Lifecycle[A]) Release(a A) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero A
	if a == zero || s.active != a {
		return false
	}
	s.active = zero
	s.exited = true
	return true
}
func (s *Lifecycle[A]) End() A {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.active
	var zero A
	s.active = zero
	s.exited = true
	return a
}
