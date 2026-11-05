package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestFindSessionDaemons(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ pid, exe, cmd string }{
		{"123", "/old/sshore (deleted)", "sshore\x00daemon\x00build\x00"},
		{"124", "/bin/sshore", "sshore\x00bridge\x00build\x00"},
		{"125", "/bin/unrelated", "unrelated\x00daemon\x00build\x00"},
	} {
		dir := filepath.Join(root, tc.pid)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tc.exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(tc.cmd), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte("SSHORE_RUNTIME_DIR=/custom/runtime\x00"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findSessionDaemons(root)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v: %v", got, err)
	}
	if got[0].pid != 123 || got[0].socket != "/custom/runtime/build.sock" {
		t.Fatal(got)
	}
}

func TestDaemonSocketEnvironment(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", "/tmp/sshore-42/main.sock"},
		{"XDG_RUNTIME_DIR=/run/user/42\x00", "/run/user/42/sshore/main.sock"},
		{"SSHORE_RUNTIME_DIR=/custom\x00XDG_RUNTIME_DIR=/ignored\x00", "/custom/main.sock"},
		{"SSHORE_RUNTIME_DIR=relative\x00", ""},
	} {
		if got := daemonSocket("main", []byte(tc.env), 42); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func TestProbeSessionDaemon(t *testing.T) {
	for _, tc := range []struct {
		state    string
		pid      int
		attached bool
	}{
		{"starting", 0, false}, {"detached", 123, false}, {"attached", 123, true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "s")
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan error, 1)
			go func() {
				c, err := l.Accept()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(time.Second))
				_, err = readMessage(c)
				if err == nil {
					err = writeMessage(c, message{Type: "session", PID: tc.pid, Attached: tc.attached})
				}
				done <- err
			}()
			got := probeSessionDaemon(daemonProcess{pid: os.Getpid(), name: "main", socket: socket}, time.Now().Add(time.Second))
			if got.state != tc.state {
				t.Fatal(got)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// Another process owning the same path cannot validate this daemon.
			got = probeSessionDaemon(daemonProcess{pid: os.Getpid() + 1, socket: socket}, time.Now().Add(time.Second))
			if got.state != "suspect" {
				t.Fatal(got)
			}
		})
	}
	for _, p := range []daemonProcess{{pid: 1}, {pid: 1, socket: filepath.Join(t.TempDir(), strconv.Itoa(os.Getpid()))}} {
		got := probeSessionDaemon(p, time.Now().Add(time.Second))
		want := "unverified"
		if p.socket != "" {
			want = "suspect"
		}
		if got.state != want {
			t.Fatal(got)
		}
	}
}
