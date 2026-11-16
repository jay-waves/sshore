package tmux

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

func TestDetachHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		_, body, err := sshio.ReadFrame(server)
		if err != nil {
			done <- err
			return
		}
		var m detachMessage
		if err = json.Unmarshal(body, &m); err != nil {
			done <- err
			return
		}
		if m.Type != "detach" {
			done <- net.ErrClosed
			return
		}
		if err = writeControl(server, detachMessage{Type: "detached", Name: "work"}); err != nil {
			done <- err
			return
		}
		_, body, err = sshio.ReadFrame(server)
		if err == nil {
			err = json.Unmarshal(body, &m)
			if err == nil && m.Type != "bye" {
				err = net.ErrClosed
			}
		}
		done <- err
	}()
	confirmed := ""
	if err := Detach(client, func(name string) { confirmed = name }); err != nil {
		t.Fatal(err)
	}
	if confirmed != "work" {
		t.Fatal("lost confirmed name")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

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

func TestStartupModes(t *testing.T) {
	for _, tc := range []struct {
		platform, path string
		noLogin        bool
		want           []string
		fail           bool
	}{
		{"linux", "/bin/bash", false, []string{"-l", "-i"}, false},
		{"linux", "/bin/zsh", false, []string{"-l", "-i"}, false},
		{"linux", "/bin/fish", true, []string{"-i"}, false},
		{"linux", "/bin/sh", false, []string{"-l", "-i"}, false},
		{"linux", "/custom/shell", false, nil, true},
		{"windows", `C:\Program Files\PowerShell\7\pwsh.exe`, true, nil, true},
		{"windows", `C:\Windows\PowerShell.EXE`, true, nil, true},
		{"windows", "pwsh.exe", false, nil, false},
		{"windows", "cmd.exe", false, nil, false},
		{"windows", "cmd.exe", true, nil, true},
	} {
		got, err := startupArgs(tc.platform, tc.path, tc.noLogin, false)
		if (err != nil) != tc.fail || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %q, %v", tc, got, err)
		}
	}
}

func TestNoProfileModes(t *testing.T) {
	for _, tc := range []struct {
		platform, path string
		want           []string
	}{
		{"linux", "bash", []string{"--noprofile", "--norc", "-i"}},
		{"linux", "sh", []string{"-i"}},
		{"linux", "zsh", []string{"-f", "-i"}},
		{"linux", "fish", []string{"--no-config", "-i"}},
		{"windows", "pwsh.exe", []string{"-NoProfile"}},
		{"windows", "cmd.exe", []string{"/D"}},
	} {
		got, err := startupArgs(tc.platform, tc.path, false, true)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
	if _, err := startupArgs("linux", "unknown", false, true); err == nil {
		t.Fatal("unsupported shell accepted")
	}
}
