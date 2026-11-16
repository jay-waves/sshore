//go:build linux && sshoretest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jay-waves/sshore/internal/sshtest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalDaemonOwnsForwardTransports(t *testing.T) {
	e := newEnv(t)
	state := filepath.Join(e.dir, "shared-forward")
	t.Cleanup(func() {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(state, "daemon", "local.json")); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("local daemon survived idle shutdown after all tasks ended")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_SSH=/missing/initial-ssh")
	run := func(environment []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, e.bin, args...)
		cmd.Env = environment
		cmd.Dir = e.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return string(out)
	}
	run(env, "launch", "--local", "keeper", "--detach")
	defer run(env, "cancel", "--local", "keeper")
	raw, err := os.ReadFile(filepath.Join(state, "daemon", "local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manager localShellRecord
	if err = json.Unmarshal(raw, &manager); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		observation := filepath.Join(e.dir, name+"-parent")
		stub := filepath.Join(e.dir, name+"-ssh")
		script := "#!/bin/sh\nprintf 'sshore-forward-ready\n'\nexec cat >/dev/null\n"
		if err = os.WriteFile(stub, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		stub = sshtest.Wrap(t, stub, env)
		out := run(append(append([]string(nil), env...), "SSHORE_TEST_CONFIG_OWNER="+observation, "SSHORE_SSH="+stub), "forward", "8080", "fixture:3000")
		_, started, ok := strings.Cut(out, "Started forward/")
		if !ok {
			t.Fatalf("missing auto-assigned forward name: %s", out)
		}
		assigned, _, ok := strings.Cut(started, ":")
		if !ok || validSession(assigned) != nil {
			t.Fatalf("invalid forward name: %s", out)
		}
		defer run(env, "cancel", "forward/"+assigned)
		raw, err = os.ReadFile(observation)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != fmt.Sprintf("%d\n%s\n", manager.ManagerPID, e.dir) {
			t.Fatalf("forward has a wrapper process or lost cwd: %s", raw)
		}
	}
}

func TestLocalDaemonIgnoresOrphanedVersion(t *testing.T) {
	e := newEnv(t)
	for _, old := range []string{"0.0.1", "999.0.0", "unknown"} {
		t.Run(old, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("SSHORE_STATE_DIR", state)
			path, err := daemonPath()
			if err != nil {
				t.Fatal(err)
			}
			r := localShellRecord{Name: "daemon", Address: "127.0.0.1:1", Token: strings.Repeat("a", 64), Version: old}
			data, _ := json.Marshal(r)
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			// A live owner must still reject the mismatch, even if it cannot answer.
			owner, err := daemonFileLock("owner")
			if err != nil {
				t.Fatal(err)
			}
			_, err = ensureLocalDaemon()
			owner.Close()
			if err == nil {
				t.Fatal("live incompatible owner was replaced")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			env := append(e.env, "SSHORE_STATE_DIR="+state, "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=")
			cmd := exec.CommandContext(ctx, e.bin, "spawn", "--local", "fresh", "--detach")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("orphaned record blocked spawn: %v %s", err, out)
			}
			defer func() {
				cmd := exec.CommandContext(ctx, e.bin, "cancel", "--all")
				cmd.Env, cmd.Stdin = env, strings.NewReader("cancel\n")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("cleanup: %v %s", err, out)
				}
			}()
			current, err := readLocalRecord(path, "daemon")
			if err != nil || current.Version != version || current.Token == r.Token {
				t.Fatalf("daemon was not replaced: %+v %v", current, err)
			}
		})
	}
}
