//go:build linux

package tmux

import (
	"testing"
	"time"
)

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}
func TestBashStateAndOutput(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a, err := b.Run("x=42; cd /; printf one; printf error >&2; false", "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, a.Done)
	await(t, a.OutputDone)
	r, err := a.Result()
	if err != nil || r.ExitCode != 1 || string(r.Stdout) != "one" || string(r.Stderr) != "error" {
		t.Fatalf("%+v %v", r, err)
	}
	a, err = b.Run("printf '%s:%s' \"$x\" \"$PWD\"", "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, a.Done)
	await(t, a.OutputDone)
	r, err = a.Result()
	if err != nil || string(r.Stdout) != "42:/" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestBashBackgroundOutput(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a, err := b.Run("(sleep .1; printf later) & printf now", "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, a.Done)
	r, _ := a.Result()
	if !r.Complete || r.OutputClosed {
		t.Fatalf("%+v", r)
	}
	await(t, a.OutputDone)
	r, err = a.Result()
	if err != nil || string(r.Stdout) != "nowlater" || !r.OutputClosed {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBashInputAndExit(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a, err := b.Run("read -r value; printf '%s' \"$value\"", "hello\n")
	if err != nil {
		t.Fatal(err)
	}
	await(t, a.Done)
	await(t, a.OutputDone)
	r, err := a.Result()
	if err != nil || string(r.Stdout) != "hello" {
		t.Fatalf("%+v %v", r, err)
	}
	a, err = b.Run("exit 9", "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, a.Done)
	r, err = a.Result()
	if err == nil || r.Complete {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBashStreamsBeforeCompletion(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	execution, err := b.Run("printf ready; sleep .3; printf done", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		result, err := execution.Result()
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Stdout) == "ready" {
			if result.Complete {
				t.Fatal("execution completed before streaming output")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no streaming output: %+v", result)
		}
		time.Sleep(5 * time.Millisecond)
	}
	await(t, execution.Done)
	await(t, execution.OutputDone)
	result, err := execution.Result()
	if err != nil || string(result.Stdout) != "readydone" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestCloseInterruptsBusyBash(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	execution, err := b.Run("while :; do :; done", "")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close blocked")
	}
	await(t, execution.Done)
	result, err := execution.Result()
	if err == nil || result.Complete {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestBashRejectsConcurrentExecution(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.Run("while :; do :; done", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run("printf unexpected", ""); err == nil {
		t.Fatal("accepted execution while busy")
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, first.Done)
}

func TestBashRejectsAfterControlCorruption(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	execution, err := b.Run("printf 'invalid\\n' >&3", "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, execution.Done)
	if _, err = execution.Result(); err == nil {
		t.Fatal("invalid control message accepted")
	}
	if _, err = b.Run("printf must-not-run", ""); err == nil {
		t.Fatal("broken session accepted work")
	}
}

func TestBashExitDoesNotWaitForInheritedControl(t *testing.T) {
	b, err := StartBash()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	execution, err := b.Run("sleep .5 & exit 9", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-execution.Done:
		result, err := execution.Result()
		if err == nil || result.Complete {
			t.Fatalf("%+v %v", result, err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("shell exit waited for descendant to close control descriptor")
	}
}
