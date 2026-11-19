package tmux

import (
	"net"
	"reflect"
	"testing"

	"github.com/jay-waves/sshore/internal/wire"
)

func TestDetachHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		if _, err := expectDetachMessage(server, "detach"); err != nil {
			done <- err
			return
		}
		if err := wire.WriteMessage(server, "detached", nil, terminalMessage{Type: "detached", Name: "work"}); err != nil {
			done <- err
			return
		}
		_, err := expectDetachMessage(server, "bye")
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
