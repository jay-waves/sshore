package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
	"github.com/jay-waves/sshore/internal/sshtest"
)

func TestSpawnRouteKeepsOwnerAndDrainsExistingEndpoint(t *testing.T) {
	parentControl, frontend := net.Pipe()
	defer parentControl.Close()
	defer frontend.Close()
	_ = frontend.SetDeadline(time.Now().Add(5 * time.Second))
	setup, launcher := net.Pipe()
	defer setup.Close()
	defer launcher.Close()
	_ = launcher.SetDeadline(time.Now().Add(5 * time.Second))

	parent := &attachment{control: parentControl}
	shell := &remoteShell{instance: strings.Repeat("a", 32), done: make(chan struct{})}
	shell.active = parent
	child := strings.Repeat("b", 32)
	peers := make(chan net.Conn, 3)
	open := func() (net.Conn, error) {
		a, b := net.Pipe()
		peers <- b
		return a, nil
	}
	ownerDone := make(chan struct{})
	endOwner := sync.OnceFunc(func() { close(ownerDone) })
	defer endOwner()
	routeDone := make(chan error, 1)
	go func() {
		ready := message{Type: "setup_ready", SoftwareVersion: version, Instance: child}
		routeDone <- shell.serveSpawnRoute(setup, parent, &spawnConfig{Local: true}, open, ready, ownerDone, false)
	}()
	if m := recv(t, launcher); m.Type != "setup_ready" || m.Instance != child {
		t.Fatalf("startup: %+v", m)
	}
	route := recv(t, frontend)
	if route.Type != "spawn_route" || route.Token == "" {
		t.Fatalf("route: %+v", route)
	}
	request := message{Token: route.Token, Instance: shell.instance, SoftwareVersion: version}
	connect := func(request message) (net.Conn, message) {
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close() })
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			defer server.Close()
			shell.connectSpawnRoute(server, request)
		}()
		return client, recv(t, client)
	}
	stale := request
	stale.Instance = strings.Repeat("c", 32)
	if _, reply := connect(stale); reply.Type != "error" {
		t.Fatalf("stale parent: %+v", reply)
	}
	stale = request
	stale.SoftwareVersion = "0.0.0-old"
	if _, reply := connect(stale); reply.Type != "error" {
		t.Fatalf("stale release: %+v", reply)
	}
	first, reply := connect(request)
	if reply.Type != "route_connected" {
		t.Fatalf("initial endpoint: %+v", reply)
	}
	(<-peers).Close()
	first.Close()

	confirm, confirmServer := net.Pipe()
	defer confirm.Close()
	defer confirmServer.Close()
	_ = confirm.SetDeadline(time.Now().Add(5 * time.Second))
	go shell.confirmSpawnRoute(confirmServer, request)
	if m := recv(t, confirm); m.Type != "route_confirmed" {
		t.Fatalf("confirmation: %+v", m)
	}
	if m := recv(t, launcher); m.Type != "handoff_accepted" {
		t.Fatalf("launcher: %+v", m)
	}
	launcher.Close()
	second, reply := connect(request)
	if reply.Type != "route_connected" {
		t.Fatalf("reopened endpoint: %+v", reply)
	}
	peer := <-peers
	defer peer.Close()
	endOwner()
	if _, reply := connect(request); reply.Type != "error" {
		t.Fatalf("route after owner ended: %+v", reply)
	}
	if len(peers) != 0 {
		t.Fatal("rejected request dialed the child")
	}
	go func() { _, _ = io.WriteString(peer, "exit tail"); peer.Close() }()
	if tail, err := io.ReadAll(second); err != nil || string(tail) != "exit tail" {
		t.Fatalf("lost output: %q %v", tail, err)
	}
	select {
	case err := <-routeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drained route did not close")
	}
}

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
			env := append(e.env, "SSHORE_STATE_DIR="+state, "HOME="+home, "SSHORE_DISABLED=", "SSHORE_LOCAL_SESSION=", "SSHORE_SOCKET=", "SSHORE_TREE_ID=original")
			t.Setenv("SSHORE_STATE_DIR", state)
			if _, err := ensureLocalDaemonContext(context.Background(), e.bin); err != nil {
				t.Fatal(err)
			}
			saveShellTree("original", "work", nil, "attached", "persistent")
			args := []string{"fixture-" + tc.command, "self:", "temporary", "--detach"}
			if remote {
				ssh := filepath.Join(e.dir, "ssh")
				if err := os.WriteFile(ssh, []byte("#!/bin/sh\nunset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN SSH_ASKPASS\nfor arg do last=$arg; done\nexec sh -c \"$last\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ssh = sshtest.Wrap(t, ssh, env)
				env = append(env, "SSHORE_SSH="+ssh)
				args = []string{"fixture-" + tc.command, "fake-host", "temporary", "--detach", "--server", e.bin}
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
				if !strings.Contains(out, "Started ") || !strings.Contains(out, "READY:0") {
					t.Fatalf("startup: %s\n%s", out, stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("startup timeout")
			}
			inspect := func() ([]byte, error) {
				args := []string{"list", "self:", "--json"}
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
				if len(inv.Shells) != 1 || !inv.Shells[0].Temporary || inv.Shells[0].Lifetime != "heartbeat" || inv.Shells[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = inv.Shells[0].PID
			} else {
				var rows []localShellView
				if err = json.Unmarshal(out, &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || !rows[0].Temporary || rows[0].Lifetime != "parent" || rows[0].State != "detached" {
					t.Fatalf("unexpected shells: %s", out)
				}
				pid = rows[0].PID
			}
			if pid <= 0 {
				t.Fatal("spawn did not start its shell")
			}
			// Reattach/detach over the regular endpoint; the original parent still owns it.
			var open func(context.Context) (net.Conn, error)
			if remote {
				t.Setenv("SSHORE_RUNTIME_DIR", e.dir)
				path, err := resolveSession("temporary")
				if err != nil {
					t.Fatal(err)
				}
				open = func(ctx context.Context) (net.Conn, error) {
					c, err := dialHostedTask(ctx, path, time.Second)
					if err != nil {
						return nil, err
					}
					_ = c.SetDeadline(time.Time{})
					return c, nil
				}
			} else {
				t.Setenv("SSHORE_STATE_DIR", state)
				open = func(ctx context.Context) (net.Conn, error) {
					c, _, err := openNativeShell(ctx, "temporary", "")
					if err != nil {
						return nil, err
					}
					_ = c.SetDeadline(time.Time{})
					return c, nil
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, err := open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			terminal, token, err := openTerminalEndpoint(ctx, options{reusable: &clientTransport{open: open}})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			control := conn.(attachmentControl)
			if err := clientAttachment(control); err != nil {
				t.Fatal(err)
			}
			if err = writeMessage(control, message{Type: "attach", Token: token, Tree: []shellTreeNode{{Identity: "other", Host: "elsewhere", Name: "other"}}, Cols: 80, Rows: 24, Term: "xterm"}); err != nil {
				t.Fatal(err)
			}
			ready, err := readMessage(control)
			if err != nil || ready.Type != "ready" || !ready.Temporary || ready.PID != pid {
				t.Fatalf("reattach: %+v %v", ready, err)
			}
			if len(ready.Tree) == 0 {
				t.Fatal("missing relationship metadata")
			}
			self := ready.Tree[len(ready.Tree)-1]
			if self.Owner != "original" || self.Parent != "other" || self.Lifetime != shellLifetime(true, remote) {
				t.Fatalf("reattach changed lifetime owner: %+v", ready.Tree)
			}
			go io.Copy(io.Discard, terminal)
			if err = detachAttachment(control); err != nil {
				t.Fatal(err)
			}
			conn.Close()
			input.Close()
			_ = parent.Wait()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := proc.LiveIdentity(pid); err != nil {
					if remote {
						waitExitReceipt(t, e.dir, "temporary", "spawn lifetime connection closed")
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("spawn shell %d survived parent exit", pid)
		})
	}
}
