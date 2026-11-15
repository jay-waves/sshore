package mux

import (
	"errors"
	"sync"
	"testing"
)

func TestAttachmentOwnership(t *testing.T) {
	var s Lifecycle[*int]
	a, b := new(int), new(int)
	fail := errors.New("prepare failed")
	if err := s.Attach(a, func() error { return fail }); !errors.Is(err, fail) || s.Active() != nil {
		t.Fatal("failed prepare acquired attachment")
	}
	if err := s.Attach(a, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(b, func() error { t.Fatal("prepared competing attachment"); return nil }); err == nil {
		t.Fatal("accepted competing attachment")
	}
	if s.Detach(b) || s.Release(b) || s.Active() != a {
		t.Fatal("stale identity changed owner")
	}
	if !s.Detach(a) || s.Release(a) || s.Exited() {
		t.Fatal("detach ended session")
	}
	if err := s.Attach(b, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !s.Release(b) || !s.Exited() {
		t.Fatal("transport loss did not end owner")
	}
	if err := s.Attach(a, func() error { return nil }); err == nil {
		t.Fatal("reattached ended session")
	}
}
func TestConcurrentAttach(t *testing.T) {
	var s Lifecycle[*int]
	var wg sync.WaitGroup
	wins := make(chan *int, 16)
	for range 16 {
		wg.Go(func() {
			a := new(int)
			if s.Attach(a, func() error { return nil }) == nil {
				wins <- a
			}
		})
	}
	wg.Wait()
	close(wins)
	n := 0
	for a := range wins {
		n++
		if s.Active() != a {
			t.Fatal("wrong owner")
		}
	}
	if n != 1 {
		t.Fatalf("%d winners", n)
	}
	if s.End() == nil || !s.Exited() || s.Active() != nil {
		t.Fatal("end did not clear attachment")
	}
}
