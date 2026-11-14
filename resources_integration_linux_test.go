package main

// Uses the existing sshoretest-tagged binary and isolated daemon fixture. The
// SSH wrapper runs only fixture commands; no real account sessions or hosts.
import (
	"context"
	"encoding/json"
	"github.com/creack/pty"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApprovedForwardIndependent(t *testing.T) {
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	e := newEnv(t)
	remoteHome := t.TempDir()
	e.env = append(e.env, "HOME="+remoteHome)
	b, pid := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	sendTest(t, b.in, message{Type: "bye"})
	b.close()
	wrapper := filepath.Join(e.dir, "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nfor last; do :; done\nexec /bin/sh -c \"$last\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := append(e.env, "SSHORE_SSH="+wrapper)
	id, err := writeApproval(options{host: "fixture", session: "main", server: e.bin, resumePID: pid}, []operation{{Kind: "forward", TargetPort: 31235}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	publishPending := func(id string) {
		t.Helper()
		dir := filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir))
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(pendingRemoteTask(id, []operation{{Kind: "forward", TargetPort: 31235}}))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, id+".json"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	publishPending(id)
	out, err := run("list")
	if err != nil || !strings.Contains(out, "approval/"+id) || !strings.Contains(out, "pending") {
		t.Fatal(out, err)
	}
	name := defaultForwardName(forwardConfig{Host: "fixture", Port: 31234, Target: 31235})
	defer run("cancel", "forward/"+name)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "review", "approval/"+id)
	cmd.Env = env
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 160})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer cmd.Process.Kill()
	output := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		answered := false
		for {
			n, e := terminal.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if !answered && strings.Contains(b.String(), "Local listening port") {
					io.WriteString(terminal, "31234\r")
					answered = true
				}
			}
			if e != nil {
				break
			}
		}
		output <- b.String()
	}()
	if err = cmd.Wait(); err != nil {
		terminal.Close()
		t.Fatalf("approve: %v %s", err, <-output)
	}
	out = <-output
	if !strings.Contains(out, "Started forward/"+name) {
		t.Fatal(out)
	}
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("source shell was replaced")
	}
	sendTest(t, resumed.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, resumed.tty, "exit")
	resumed.close()
	out, err = run("list")
	if err != nil || !strings.Contains(out, "forward/"+name) || !strings.Contains(out, "running") || strings.Contains(out, "approval/"+id) {
		t.Fatal(out, err)
	}
	if _, e := os.Stat(filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir), id+".json")); !os.IsNotExist(e) {
		t.Fatal("approved task mirror survived", e)
	}
	out, err = run("list", "--json")
	var snapshot inventory
	if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil || len(snapshot.Resources) != 1 || snapshot.Resources[0].Target != "unchecked" {
		t.Fatal(out, err)
	}
	out, err = run("list", "fixture", "--json", "--server", e.bin)
	if err != nil || json.Unmarshal([]byte(out), &snapshot) != nil || len(snapshot.Exits) == 0 {
		t.Fatal(out, err)
	}
	// The independent watcher synchronizes only this resource to the peer list.
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, err = run("list")
		if err != nil {
			t.Fatal(out, err)
		}
		if strings.Contains(out, "peer/") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing peer task", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Duplicate identity cannot silently replace an independent resource.
	if out, err = run("forward", "origin:31234", "fixture:31235"); err == nil {
		t.Fatal("duplicate accepted", out)
	}
	if out, err = run("cancel", "forward/"+name); err != nil {
		t.Fatal(out, err)
	}
	out, err = run("list")
	if err != nil || strings.Contains(out, "forward/"+name) {
		t.Fatal(out, err)
	}
	rejected, e2 := writeApproval(options{host: "fixture", session: "main", server: e.bin, resumePID: pid}, []operation{{Kind: "forward", TargetPort: 31235}})
	if e2 != nil {
		t.Fatal(e2)
	}
	publishPending(rejected)
	if out, err = run("cancel", "approval/"+rejected); err != nil {
		t.Fatal(out, err)
	}
	if _, e2 = os.Stat(filepath.Join(remoteHome, filepath.FromSlash(remoteTaskRelativeDir), rejected+".json")); !os.IsNotExist(e2) {
		t.Fatal("rejected mirror survived", e2)
	}

}
