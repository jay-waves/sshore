package tests_test

import (
	"context"
	"encoding/json"
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

func TestWindowsKillAllWaitsForIdleDaemonExit(t *testing.T) {
	e := newCLI(t)
	state := filepath.Join(e.dir, "state")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = append(e.env, "SHELL=cmd.exe")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run("fixture-launch", "--local", "idle", "--detach")
	run("kill", "self:idle")
	data, err := os.ReadFile(filepath.Join(state, "daemon", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ ManagerPID uint32 }
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, record.ManagerPID)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	run("kill", "--all")
	if status, err := windows.WaitForSingleObject(handle, 0); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("daemon has not exited: wait status %d, error %v", status, err)
	}
}

func TestWindowsLocalDetachKeepsShell(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Niubash bash executable unavailable")
	}
	e := newCLI(t)
	e.env = append(e.env, "SHELL="+shell, "PS1=SSHORE_OUTER> ")
	outer, err := tmux.Start(tmux.Config{Path: shell, Args: []string{"--noprofile", "--norc", "-i"}, Env: e.env, Size: tmux.Size{Cols: 160, Rows: 40}})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the outer console alive until the fixture has cancelled its daemon.
	defer outer.Close()
	defer func() {
		cmd := exec.Command(e.bin, "kill", "--all")
		cmd.Env, cmd.Stdin = e.env, strings.NewReader("kill\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("cleanup: %v: %s", err, out)
		}
	}()
	chunks := make(chan string, 64)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := outer.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var transcript strings.Builder
	pending := ""
	wait := func(marker string) {
		t.Helper()
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			if i := strings.Index(pending, marker); i >= 0 {
				pending = pending[i+len(marker):]
				return
			}
			select {
			case text, ok := <-chunks:
				if !ok {
					t.Fatalf("console ended waiting for %q: %s", marker, transcript.String())
				}
				transcript.WriteString(text)
				pending += text
			case <-timer.C:
				t.Fatalf("waiting for %q: %s", marker, transcript.String())
			}
		}
	}
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(outer, s); err != nil {
			t.Fatal(err)
		}
	}
	bin := "'" + strings.ReplaceAll(filepath.ToSlash(e.bin), "'", "'\"'\"'") + "'"
	wait("SSHORE_OUTER>")
	send(fmt.Sprintf("%s fixture-launch --local nativecheck; printf 'OUTER_%%s\\n' detached\r", bin))
	wait("Connected self:nativecheck")
	send("PS1='SSHORE_INNER> '\r")
	wait("SSHORE_INNER>")
	send("SSHORE_NAV=OX\x1b[D\x1b[3~K\r")
	send("printf 'EDIT_%s\\n' \"$SSHORE_NAV\"\r")
	wait("EDIT_OK")
	send("\x02d")
	wait("OUTER_detached")
	rows := e.shells(t)
	if len(rows) != 1 || rows[0].PID == 0 || rows[0].State != "detached" {
		t.Fatalf("detach lost shell: %+v", rows)
	}
	pid := rows[0].PID
	send(fmt.Sprintf("%s attach self:%s; printf 'OUTER_%%s\\n' exited\r", bin, rows[0].ShortID))
	wait("SSHORE_INNER>")
	rows = e.shells(t)
	if len(rows) != 1 || rows[0].PID != pid || rows[0].State != "attached" {
		t.Fatalf("reattach changed shell: %+v", rows)
	}
	send("exit\r")
	wait("OUTER_exited")
}
