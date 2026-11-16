package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalNoLoginStartup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	e := newEnv(t)
	home := t.TempDir()
	marker := filepath.Join(home, "startup")
	for file, value := range map[string]string{".bash_profile": "login", ".bashrc": "interactive"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("printf "+value+" > "+shellQuote(marker)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := append(e.env, "HOME="+home, "SHELL="+bash, "SSHORE_SOCKET=", "SSHORE_STATE_DIR="+filepath.Join(e.dir, "local-state"))
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(e.bin, args...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("launch", "--local", "startup", "--no-login", "--detach")
	defer run("cancel", "--local", "startup")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if string(data) != "interactive" {
				t.Fatalf("wrong startup: %q", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoProfileRemoteStartup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip(err)
	}
	e := newEnv(t)
	home := t.TempDir()
	for _, file := range []string{".bash_profile", ".bashrc"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("exit 77\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e.env = append(e.env, "SHELL="+bash, "HOME="+home, "SSHORE_PROFILE_INHERITED=kept")
	b := e.bridge("clean")
	defer b.close()
	sendTest(t, b.in, message{Type: "attach", Term: "xterm", Cols: 80, Rows: 24, NoProfile: true})
	m := recv(t, b.out)
	if m.Type != "ready" {
		t.Fatal(m)
	}
	process, err := os.FindProcess(m.PID)
	if err != nil {
		t.Fatal(err)
	}
	e.processes = append(e.processes, process)
	b.tty, err = b.mux.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if head := recv(t, b.tty); head.Type != "terminal" {
		t.Fatal(head)
	}
	shellOutput(t, b, `printf 'PROFILE-%s' "$SSHORE_PROFILE_INHERITED"`, "PROFILE-kept")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}
