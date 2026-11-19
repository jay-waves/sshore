package tmux

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestConPTYInputPipeStopsBlockedWriteBeforeNewOwner(t *testing.T) {
	console, writer, err := consolePipe(false)
	if err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(console), "conpty-input-reader")
	defer writer.Close()
	defer reader.Close()

	old := NewInputLease(writer)
	writing := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		payload := bytes.Repeat([]byte{'x'}, 4<<20)
		close(writing)
		_, err := old.Write(payload)
		written <- err
	}()
	<-writing
	select {
	case err := <-written:
		t.Fatalf("write completed without a reader: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	stopped := make(chan error, 1)
	go func() { stopped <- old.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input lease did not cancel the blocked pipe write")
	}
	if err := <-written; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write error %v, want deadline", err)
	}
	if _, err := old.Write([]byte("STALE")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retired lease accepted a write: %v", err)
	}

	drained := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		drained <- data
	}()
	fresh := NewInputLease(writer)
	go func() {
		_, err := fresh.Write([]byte("NEW"))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement lease could not write")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-drained:
		if !bytes.HasSuffix(data, []byte("NEW")) || bytes.Contains(data, []byte("STALE")) {
			t.Fatalf("unexpected data after handoff: %d bytes", len(data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipe reader did not reach EOF")
	}
}

func TestConPTYOutputPipeReadDeadline(t *testing.T) {
	console, reader, err := consolePipe(true)
	if err != nil {
		t.Fatal(err)
	}
	writer := os.NewFile(uintptr(console), "conpty-output-writer")
	defer reader.Close()
	defer writer.Close()

	if err := reader.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := reader.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error %v, want deadline", err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("END")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "END" {
		t.Fatalf("final output %q: %v", buf, err)
	}
	if _, err := reader.Read(buf); err != io.EOF {
		t.Fatalf("read error %v, want EOF", err)
	}
}
