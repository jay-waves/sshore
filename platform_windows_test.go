package main

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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

func TestResourceHiddenConsoleInheritance(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestResourceConsoleChild$")
	detachResourceWorker(cmd)
	cmd.Env = append(os.Environ(), "SSHORE_TEST_CONSOLE_CHILD=worker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hidden console: %v\n%s", err, out)
	}
}

func TestResourceConsoleChild(t *testing.T) {
	role := os.Getenv("SSHORE_TEST_CONSOLE_CHILD")
	if role == "" {
		t.Skip("subprocess fixture")
	}
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if hwnd == 0 {
		t.Fatal("worker and askpass must share an allocated hidden console")
	}
	visible, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible").Call(hwnd)
	if visible != 0 {
		t.Fatal("background console is visible")
	}
	if role == "worker" {
		// OpenSSH launches a console-subsystem askpass executable without our Go
		// creation flags. Reproduce that child launch, not just the worker's flags.
		cmd := exec.Command(os.Args[0], "-test.run=^TestResourceConsoleChild$")
		cmd.Env = append(os.Environ(), "SSHORE_TEST_CONSOLE_CHILD=askpass")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("askpass console: %v\n%s", err, out)
		}
	}
}
