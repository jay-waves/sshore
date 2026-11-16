package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"

	transportio "github.com/jay-waves/sshore/internal/transport"
)

func muxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, transportio.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
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
			if err == nil || !strings.Contains(err.Error(), "remote shell daemon") || !strings.Contains(err.Error(), "new shell name") {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAttachmentRejectsTunnelStreams(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveApprovedStreams(ctx, server)
	for _, kind := range []string{"forward_connect", "forward_control"} {
		st, e := client.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		st.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(st, message{Type: kind, Action: "add"}); e != nil {
			t.Fatal(e)
		}
		if m, e := readMessage(st); e == nil {
			t.Fatalf("obsolete attachment tunnel accepted: %+v", m)
		}
		st.Close()
	}
}

func TestApprovalMismatchDoesNotWriteFile(t *testing.T) {
	client, server := muxPair(t)
	target := filepath.Join(t.TempDir(), "secret")
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "offer", Name: "other", Size: 3})
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "download", Path: "/tmp/a", Name: "a", Size: 3, LocalPath: target}, false)
	if err == nil {
		t.Fatal("mismatch accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("wrote unapproved file", err)
	}
}

func TestChangedUploadIsNotSent(t *testing.T) {
	client, server := muxPair(t)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			result <- err
			return
		}
		defer st.Close()
		_, err = readMessage(st)
		result <- err
	}()
	err := executeApprovedFile(context.Background(), client, operation{Kind: "upload", Path: "/tmp", Name: "source", Size: 1, LocalPath: path}, true)
	if err == nil {
		t.Fatal("changed upload accepted")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unapproved upload sent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not close")
	}
}
