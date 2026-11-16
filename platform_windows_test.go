package main

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLocalShellPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"shell", []string{`SHELL=C:\Program Files\nu\nu.exe`, `COMSPEC=C:\Windows\System32\cmd.exe`}, `C:\Program Files\nu\nu.exe`},
		{"case and override", []string{`SHELL=old.exe`, `Shell=nu.exe`}, `nu.exe`},
		{"comspec", []string{`SHELL=`, `ComSpec=custom.exe`}, `custom.exe`},
		{"default", nil, `cmd.exe`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := localShellPath(tc.env)
			if path != tc.want {
				t.Fatalf("got %q, want %q", path, tc.want)
			}
		})
	}
}

func TestWindowsSpawnOwnerClosesTerminal(t *testing.T) {
	parent := exec.Command("cmd.exe", "/d", "/q")
	input, err := parent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err = parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { parent.Process.Kill(); parent.Wait() }()
	identity, err := spawnProcessIdentity(parent.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	s := &localShell{environment: append(os.Environ(), "SHELL=cmd.exe"), record: localShellRecord{Directory: t.TempDir(), Owner: spawnOwner{PID: parent.Process.Pid, Start: identity}}, done: make(chan struct{})}
	defer s.close()
	if err = s.start(message{Term: "xterm", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	go watchSpawnOwner(s.record.Owner, s.done, s.close)
	pid := s.terminal.PID()
	// A normal foreground client is unnecessary for the owner to retain the shell.
	if _, err = spawnProcessIdentity(pid); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(input, "exit\r\n")
	if err = parent.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn survived parent exit")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = spawnProcessIdentity(pid); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("spawn terminal process survived owner cleanup")
}
