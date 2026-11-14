package main

import (
	"fmt"
	"net"
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
