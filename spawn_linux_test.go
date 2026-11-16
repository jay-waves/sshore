//go:build linux && sshoretest

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnLifetime(t *testing.T) {
	for _, tc := range []struct {
		remote  bool
		command string
	}{{false, "spawn"}, {true, "spawn"}} {
		remote := tc.remote
		t.Run(fmt.Sprintf("%s/remote=%t", tc.command, remote), func(t *testing.T) {
			e := newEnv(t)
			state := filepath.Join(e.dir, "spawn-state")
			home := filepath.Join(e.dir, "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			env := append(e.env, "SSHORE_STATE_DIR="+state, "HOME="+home, "SSHORE_DISABLED=", "SSHORE_LOCAL_SESSION=", "SSHORE_SOCKET=")
			args := []string{tc.command, "--local", "temporary", "--detach"}
			if remote {
				ssh := filepath.Join(e.dir, "ssh")
				if err := os.WriteFile(ssh, []byte("#!/bin/sh\nunset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN SSH_ASKPASS\nfor arg do last=$arg; done\nexec sh -c \"$last\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				env = append(env, "SSHORE_SSH="+ssh)
				args = []string{tc.command, "fake-host", "temporary", "--detach", "--server", e.bin}
			}
			// This parent stays alive after the detached CLI exits, then exits on EOF.
			parent := exec.Command("sh", append([]string{"-c", `"$@"; result=$?; printf 'READY:%s\n' "$result"; read answer`, "parent", e.bin}, args...)...)
			parent.Env = env
			input, err := parent.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := parent.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			parent.Stderr = &stderr
			if err = parent.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { input.Close(); parent.Process.Kill(); parent.Wait() })
			lines := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(output)
				var b strings.Builder
				for scanner.Scan() {
					b.WriteString(scanner.Text() + "\n")
					if strings.HasPrefix(scanner.Text(), "READY:") {
						lines <- b.String()
						return
					}
				}
				lines <- b.String()
			}()
			select {
			case out := <-lines:
				if !strings.Contains(out, "Started spawn/temporary") || !strings.Contains(out, "READY:0") {
					t.Fatalf("startup: %s\n%s", out, stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("startup timeout")
			}
			inspect := func() ([]byte, error) {
				args := []string{"list", "--local", "--json"}
				if remote {
					args = []string{"list", "fake-host", "--json", "--server", e.bin}
				}
				cmd := exec.Command(e.bin, args...)
				cmd.Env = env
				return cmd.CombinedOutput()
			}
			out, err := inspect()
			if err != nil {
				t.Fatalf("list: %s %v", out, err)
			}
			var pid int
			if remote {
				var inv inventory
				if err = json.Unmarshal(out, &inv); err != nil {
					t.Fatalf("%s: %v", out, err)
				}
				if len(inv.Shells) != 1 || !inv.Shells[0].Temporary || inv.Shells[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = inv.Shells[0].PID
			} else {
				var rows []localShellView
				if err = json.Unmarshal(out, &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || !rows[0].Temporary || rows[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = rows[0].PID
			}
			if pid <= 0 {
				t.Fatal("spawn did not start its shell")
			}
			// Reattach/detach over the regular endpoint; the original parent still owns it.
			var conn io.ReadWriteCloser
			if remote {
				t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
				path, err := resolveSession("temporary")
				if err != nil {
					t.Fatal(err)
				}
				c, err := net.DialTimeout("unix", path, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				conn = c
			} else {
				t.Setenv("SSHORE_STATE_DIR", state)
				r, err := readLocalSession("temporary")
				if err != nil {
					t.Fatal(err)
				}
				c, err := dialLocalSession(r)
				if err != nil {
					t.Fatal(err)
				}
				conn = c
			}
			mx, err := clientMux(conn)
			if err != nil {
				t.Fatal(err)
			}
			control, err := mx.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if err = writeMessage(control, message{Type: "attach", Cols: 80, Rows: 24, Term: "xterm"}); err != nil {
				t.Fatal(err)
			}
			ready, err := readMessage(control)
			if err != nil || ready.Type != "ready" || !ready.Temporary || ready.PID != pid {
				t.Fatalf("reattach: %+v %v", ready, err)
			}
			terminal, err := mx.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = readMessage(terminal); err != nil {
				t.Fatal(err)
			}
			go io.Copy(io.Discard, terminal)
			if err = detachControl(control); err != nil {
				t.Fatal(err)
			}
			mx.Close()
			conn.Close()
			input.Close()
			_ = parent.Wait()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := spawnProcessIdentity(pid); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("spawn shell %d survived parent exit", pid)
		})
	}
}
