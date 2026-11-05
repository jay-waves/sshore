package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func muxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}
func taskPair(t *testing.T, client, server *smux.Session, m message) (*clientTransfer, *smux.Stream) {
	t.Helper()
	remote, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	local, err := client.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := &clientTransfer{id: m.ID, stream: local, request: m, choice: make(chan string, 1), ctx: ctx, cancel: cancel}
	t.Cleanup(func() { task.close(); _ = remote.Close() })
	return task, remote
}
func TestClientConcurrentTasksCancelIsolation(t *testing.T) {
	client, server := muxPair(t)
	dir := t.TempDir()
	down, downStream := taskPair(t, client, server, message{Type: "offer", ID: 1, Name: "partial.bin", Size: 100})
	downDone := make(chan error, 1)
	go func() { _, err := runFileTask(down); downDone <- err }()
	down.choice <- filepath.Join(dir, "partial.bin")
	m, err := readMessage(downStream)
	if err != nil || m.Type != "accept" {
		t.Fatal(m, err)
	}
	if err = writeMessage(downStream, message{Type: "chunk", ID: 1, Data: []byte("partial")}); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("upload\x00\xff"), chunkSize)
	path := filepath.Join(dir, "source.bin")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	up, upStream := taskPair(t, client, server, message{Type: "upload", ID: 2})
	upDone := make(chan error, 1)
	go func() { _, err := runFileTask(up); upDone <- err }()
	up.choice <- path
	m, err = readMessage(upStream)
	if err != nil || m.Type != "offer" {
		t.Fatal(m, err)
	}
	if err = writeMessage(upStream, message{Type: "accept", ID: 2}); err != nil {
		t.Fatal(err)
	}
	down.close()
	select {
	case err = <-downDone:
		if err == nil {
			t.Fatal("cancelled task succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock task")
	}
	var got []byte
	for {
		m, err = readMessage(upStream)
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == "end" {
			sum := sha256.Sum256(got)
			if m.Hash != hex.EncodeToString(sum[:]) {
				t.Fatal("hash mismatch")
			}
			break
		}
		if m.Type != "chunk" {
			t.Fatal(m.Type)
		}
		got = append(got, m.Data...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("sibling upload corrupted")
	}
	if err = writeMessage(upStream, message{Type: "result", ID: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-upDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sibling upload stalled")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "source.bin" {
		t.Fatal("partial file leaked", entries, err)
	}
}
func TestRemoteCancellationDuringFilePrompt(t *testing.T) {
	client, server := muxPair(t)
	task, remote := taskPair(t, client, server, message{Type: "upload", ID: 7})
	done := make(chan error, 1)
	go func() { _, err := runFileTask(task); done <- err }()
	_ = remote.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("file prompt did not observe remote cancellation")
	}
}

func TestClientMuxRejectsDifferentOrUnknownBuild(t *testing.T) {
	for _, peer := range []string{"0.0.0-old", ""} {
		t.Run(peer, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			_ = a.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() {
				m, err := readMessage(b)
				if err == nil && m.SoftwareVersion != version {
					err = fmt.Errorf("client omitted its software version")
				}
				if err == nil {
					err = writeMessage(b, message{Type: "mux_ready", SoftwareVersion: peer})
				}
				done <- err
			}()
			mux, err := clientMux(a)
			if mux != nil {
				mux.Close()
				t.Fatal("accepted incompatible daemon")
			}
			if err == nil || !strings.Contains(err.Error(), "remote session daemon") || !strings.Contains(err.Error(), "new session name") {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReleaseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.4.9", false}, {"0.4.9", "0.4.0", false}, {"0.4.0", "0.4.0", true},
		{"0.4.0", "0.5.0", false}, {"1.4.0", "0.4.0", false},
		{"0.4.0", "0.40.0", false}, {"0.4.0", "0.4.00", false},
		{"0.4.0", "0.4.1-rc1", false}, {"0.4.0", "", false},
		{"0.4.1-dev-a", "0.4.1-dev-a", true}, {"0.4.1-dev-a", "0.4.1-dev-b", false},
	} {
		if got := compatibleVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("%s / %s: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
