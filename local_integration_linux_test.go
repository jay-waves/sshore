package main

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestLocalShellDetachAndReattach(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "local-state")
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSH_CONNECTION=", "SHELL=/bin/sh", "TERM=xterm-256color")
	var id uint64
	for _, args := range [][]string{{"launch", "--name", "localtest"}, {"attach", "--local", "localtest"}} {
		if args[0] == "attach" {
			list := exec.Command(e.bin, "list", "--local", "--json")
			list.Env = env
			data, err := list.Output()
			if err != nil {
				t.Fatal(err)
			}
			var rows []localShellView
			if err = json.Unmarshal(data, &rows); err != nil || len(rows) != 1 {
				t.Fatalf("list: %s %v", data, err)
			}
			id = rows[0].ID
			rename := exec.Command(e.bin, "rename", strconv.FormatUint(id, 10), "renamed")
			rename.Env = env
			if out, err := rename.CombinedOutput(); err != nil {
				t.Fatalf("rename: %s %v", out, err)
			}
			list = exec.Command(e.bin, "list", "--local", "--json")
			list.Env = env
			data, err = list.Output()
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Name != "renamed" {
				t.Fatalf("renamed list: %s %v", data, err)
			}
			args = []string{"attach", strconv.FormatUint(id, 10)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		output := make(chan string, 1)
		go func() {
			var out strings.Builder
			buf := make([]byte, 4096)
			stage := 0
			for {
				n, err := terminal.Read(buf)
				out.Write(buf[:n])
				if stage == 0 && strings.Contains(out.String(), "Connected local /") {
					stage = 1
					if args[0] == "launch" {
						io.WriteString(terminal, "\x02c")
					} else {
						io.WriteString(terminal, "exit\n")
					}
				}
				if args[0] == "launch" && stage == 1 && strings.Contains(out.String(), "Connected local / pikachu") {
					stage = 2
					io.WriteString(terminal, "\x02l")
				}
				if args[0] == "launch" && stage == 2 && strings.Count(out.String(), "Connected local / localtest") >= 2 {
					stage = 3
					io.WriteString(terminal, "\x02d")
				}
				if err != nil {
					break
				}
			}
			output <- out.String()
		}()
		err = cmd.Wait()
		terminal.Close()
		cancel()
		out := <-output
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		if args[0] == "launch" {
			first, err := readLocalRecord(filepath.Join(state, "local-shells", "localtest.json"), "localtest")
			if err != nil {
				t.Fatal(err)
			}
			second, err := readLocalRecord(filepath.Join(state, "local-shells", "pikachu.json"), "pikachu")
			if err != nil {
				t.Fatal(err)
			}
			if first.ManagerPID <= 0 || first.ManagerPID != second.ManagerPID {
				t.Fatalf("shells do not share a daemon: %d, %d", first.ManagerPID, second.ManagerPID)
			}
			cleanup := exec.Command(e.bin, "cancel", "--local", "pikachu")
			cleanup.Env = env
			if output, err := cleanup.CombinedOutput(); err != nil {
				t.Fatalf("cleanup: %s %v", output, err)
			}
		}
		if args[0] == "launch" && !strings.Contains(out, "Detached;") {
			t.Fatal(out)
		}
		if args[0] == "attach" && !strings.Contains(out, "Exited launch/renamed on local (code 0).") {
			t.Fatalf("missing shell exit notice: %s", out)
		}
	}
}
