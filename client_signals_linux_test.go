package main

import (
	"os"
	"testing"
	"time"
)

func TestClientSignalPhases(t *testing.T) {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("initial setup cancels", func(t *testing.T) {
		ctx, _, stop := newClientSignals(false)
		defer stop()
		if err := self.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("initial setup ignored interrupt")
		}
	})
	t.Run("recovery then attachment", func(t *testing.T) {
		ctx, signals, stop := newClientSignals(true)
		defer stop()
		if err := self.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("repeated interrupt cancelled parent recovery")
		case <-time.After(50 * time.Millisecond):
		}
		signals.interactive.Store(true)
		if err := self.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-signals.interrupts:
		case <-ctx.Done():
			t.Fatal("interactive interrupt cancelled connection")
		case <-time.After(time.Second):
			t.Fatal("interactive interrupt was not forwarded")
		}
	})
}
