package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
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

func TestBackgroundWorkerHasNoConsole(t *testing.T) {
	if os.Getenv("SSHORE_TEST_CONSOLE_CHILD") == "worker" {
		hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if hwnd != 0 {
			t.Fatal("background worker has a console")
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackgroundWorkerHasNoConsole$")
	detachResourceWorker(cmd)
	cmd.Env = append(os.Environ(), "SSHORE_TEST_CONSOLE_CHILD=worker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
}

func TestWindowsPrivateStateHandle(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state")
	if err := os.WriteFile(file, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path      string
		directory bool
		valid     bool
	}{
		{dir, true, true}, {file, false, true}, {dir, false, false}, {file, true, false},
	} {
		err := privateStatePath(tc.path, tc.directory)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	before, err := windows.GetNamedSecurityInfo(file, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	broader, err := windows.SecurityDescriptorFromString(before.String() + "(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := broader.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(file, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := privateStatePath(file, false); err != nil {
			t.Fatal(err)
		}
		after, err := windows.GetNamedSecurityInfo(file, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || after.String() != before.String() {
			t.Fatalf("private ACL not restored: %v", err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Logf("symlink unavailable: %v", err)
		return
	}
	if err := privateStatePath(link, false); err == nil {
		t.Fatal("accepted reparse point")
	}
}
func TestWindowsPlatform(t *testing.T) {
	if err := checkPlatform(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSSHSecret(t *testing.T) {
	if os.Getenv("SSHORE_TEST_SECRET_CONSOLE") == "1" {
		env, stop, err := startSpawnAuth()
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		secret, err := brokerSSHSecret(env)(context.Background(), "TEST_PASSWORD")
		if err != nil || string(secret) != "test-Password!42" {
			t.Fatalf("synthetic password mismatch: length=%d error=%v", len(secret), err)
		}
		clear(secret)
		fmt.Println("SECRET_OK")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := tmux.Start(tmux.Config{Path: exe, Args: []string{"-test.run=^TestWindowsSSHSecret$"}, Env: append(os.Environ(), "SSHORE_TEST_SECRET_CONSOLE=1"), Size: tmux.Size{Cols: 100, Rows: 30}})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		sent := false
		buf := make([]byte, 4096)
		for {
			n, e := terminal.Read(buf)
			out.Write(buf[:n])
			if !sent && strings.Contains(out.String(), "TEST_PASSWORD:") {
				sent = true
				terminal.Write([]byte("test-Password!42\r"))
			}
			if e != nil {
				done <- out.String()
				return
			}
		}
	}()
	select {
	case output := <-done:
		if !strings.Contains(output, "SECRET_OK") {
			t.Fatalf("console failed: %s", output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("password input timed out")
	}
}
