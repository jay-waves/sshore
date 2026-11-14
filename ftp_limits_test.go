package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestFTPDirectoryBudgetKeepsConnection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if e := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, limit := range []string{"entries", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			a, b := net.Pipe()
			server, e := sftp.NewServer(a)
			if e != nil {
				t.Fatal(e)
			}
			go func() { _ = server.Serve(); _ = server.Close() }()
			wire := newFTPWire(b, b)
			if limit == "entries" {
				wire.maxEntries = 2
			} else {
				wire.maxDirectoryBytes = 16
			}
			client, e := sftp.NewClientPipe(wire, wire)
			if e != nil {
				t.Fatal(e)
			}
			defer server.Close()
			defer client.Close()
			_, e = client.ReadDir(filepath.ToSlash(root))
			if e == nil || !strings.Contains(e.Error(), "listing limit") {
				t.Fatalf("expected budget failure, got %v", e)
			}
			if _, e = client.Stat(filepath.ToSlash(filepath.Join(root, "a"))); e != nil {
				t.Fatalf("unrelated operation failed: %v", e)
			}
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if len(wire.directories) != 0 || len(wire.requests) != 0 {
				t.Fatalf("leaked directory/request state: %d/%d", len(wire.directories), len(wire.requests))
			}
		})
	}
}
func TestFTPRejectsOversizedPacket(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], ftpMaxPacket+1)
	wire := newFTPWire(bytes.NewReader(header[:]), nil)
	if _, e := wire.Read(make([]byte, 1)); e == nil {
		t.Fatal("accepted oversized response")
	}
}

type ftpTestMount struct{ unmount func() error }

func (m ftpTestMount) Unmount() error { return m.unmount() }
func (m ftpTestMount) Wait()          {}
func TestFTPGracefulUnmount(t *testing.T) {
	for _, failure := range []error{nil, syscall.EBUSY} {
		aborted := false
		stopped, e := stopFTPMount(ftpTestMount{func() error { return failure }}, nil, func() { aborted = true }, time.Second)
		if aborted || stopped != (failure == nil) || !errors.Is(e, failure) {
			t.Fatalf("stopped=%v aborted=%v error=%v", stopped, aborted, e)
		}
	}
}
func TestFTPForcedUnmount(t *testing.T) {
	for _, kind := range []string{"timeout", "signal"} {
		t.Run(kind, func(t *testing.T) {
			entered, released := make(chan struct{}), make(chan struct{})
			signals := make(chan os.Signal, 1)
			grace := 10 * time.Millisecond
			if kind == "signal" {
				grace = time.Second
				go func() { <-entered; signals <- os.Interrupt }()
			}
			stopped, e := stopFTPMount(ftpTestMount{func() error { close(entered); <-released; return nil }}, signals, func() { close(released) }, grace)
			if !stopped || e == nil {
				t.Fatalf("forced unmount lost error: %v %v", stopped, e)
			}
		})
	}
}
func TestFTPWireFragmentedWrites(t *testing.T) {
	var output bytes.Buffer
	wire := newFTPWire(nil, ftpWriteCloser{&output})
	packet := []byte{0, 0, 0, 5, 1, 0, 0, 0, 3}
	for _, b := range packet {
		if _, e := wire.Write([]byte{b}); e != nil {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(output.Bytes(), packet) {
		t.Fatalf("wire changed frame: %v", output.Bytes())
	}
}

type ftpWriteCloser struct{ io.Writer }

func (ftpWriteCloser) Close() error { return nil }
