package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestApprovalSourceRejectsReusedNameAndPID(t *testing.T) {
	s, request := routedResourceFixture(t)
	request.Instance = "replacement"
	if err := s.checkApprovalSource(request); err == nil {
		t.Fatal("approval accepted another instance with the same name and PID")
	}
	request.Instance = s.record.UID
	if err := s.checkApprovalSource(request); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedTransferRequiresMatchingMetadata(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	id, err := resolveSession("main")
	if err != nil {
		t.Fatal(err)
	}
	detachTestClient(t, b)
	open := func() (net.Conn, error) {
		conn, err := e.dial("main")
		if err != nil {
			return nil, err
		}
		sendTest(t, conn, message{Type: "route_files", SoftwareVersion: version, Name: "main", PID: pid, Instance: id, ApprovalID: strings.Repeat("a", 32)})
		if reply := recv(t, conn); reply.Type != "resource_ready" {
			conn.Close()
			t.Fatalf("file route: %+v", reply)
		}
		return conn, nil
	}
	source := filepath.Join(e.dir, "source")
	if err := os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.dir, "target")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = executeApprovedFile(ctx, open, newApprovedFile(operation{Kind: "download", Path: source, Name: "source", Size: 1}, target, nil), true)
	if err == nil {
		t.Fatal("changed remote file accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("unapproved file created", err)
	}
	// The same stream path succeeds for the exact approved metadata.
	if err = executeApprovedFile(ctx, open, newApprovedFile(operation{Kind: "download", Path: source, Name: "source", Size: 7}, target, nil), true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "changed" {
		t.Fatal(string(data), err)
	}
	resumed, resumedPID := e.attach("main", "attach")
	if resumedPID != pid {
		t.Fatal("file execution replaced source shell")
	}
	resumed.writeInput(t, []byte("exit\n"))
	resumed.readExit(t)
}

func TestApprovedAttachRejectsChangedSession(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	sendTest(t, b.in, message{Type: "bye"})
	b.close()
	next := e.bridge("main")
	next.sendAttach(t, message{Type: "attach", Action: "approved", PID: pid + 1, Term: "xterm", Cols: 80, Rows: 24})
	if m := recv(t, next.out); m.Type != "fatal" {
		t.Fatal("accepted wrong shell", m)
	}
	sendTest(t, next.in, message{Type: "bye"})
	next.close()
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("shell was replaced")
	}
	resumed.writeInput(t, []byte("exit\n"))
	resumed.readExit(t)
}

func TestEndpointRequestHelpers(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	sendTest(t, b.in, message{Type: "approval_ready"})
	source := filepath.Join(e.dir, "copy-source")
	if err := os.WriteFile(source, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		kind, hint string
		reverse    bool
	}{
		{[]string{"cp", "origin:", ".:" + e.dir}, "upload", "", false},
		{[]string{"cp", "origin:/client/source", ".:" + e.dir}, "upload", "/client/source", false},
		{[]string{"cp", ".:" + source, "origin:/client/target"}, "download", "/client/target", false},
		{[]string{"cp", "::", ".:" + e.dir}, "upload", "", false},
		{[]string{"cp", ".:" + source, "::/client/target"}, "download", "/client/target", false},
		{[]string{"mount", ".:" + e.dir, "origin:"}, "mount", "", false},
		{[]string{"mount", ".:" + e.dir, "origin:/client/mount", "--read-only"}, "mount", "/client/mount", false},
		{[]string{"mount", ".:" + e.dir, "::"}, "mount", "", false},
		{[]string{"mount", ".:" + e.dir, "::/client/mount", "--read-only"}, "mount", "/client/mount", false},
		{[]string{"forward", "origin:", ".:3000"}, "forward", "", false},
		{[]string{"forward", ".:3000", "origin:8080"}, "forward", "8080", true},
		{[]string{"forward", "::", ".:3000"}, "forward", "", false},
		{[]string{"forward", ".:3000", "::8080"}, "forward", "8080", true},
	} {
		_, done := e.helper(tc.args[0], tc.args[1:]...)
		m := recv(t, b.out)
		if m.Type != "approval_request" || len(m.Operations) != 1 {
			t.Fatal(m)
		}
		op := m.Operations[0]
		if op.Kind != tc.kind || op.LocalHint != tc.hint || op.Reverse != tc.reverse {
			t.Fatal(op)
		}
		if op.Kind == "mount" && (op.Path != e.dir || op.ReadOnly != (tc.hint != "")) {
			t.Fatal(op)
		}
		if op.Kind == "forward" && (!op.LocalPortPending() || op.ServerPort() != 3000) {
			t.Fatal("resolved without local consent", op)
		}
		sendTest(t, b.in, message{Type: "queued", Token: m.Token})
		if reply := recv(t, b.out); reply.Type != "queued_ack" || reply.Token != m.Token {
			t.Fatal(reply)
		}
		waitHelper(t, done, true)
	}
	b.writeInput(t, []byte("exit\n"))
	b.readExit(t)
}

func TestApprovalPrompt(t *testing.T) {
	for _, scenario := range []string{"upload", "retry", "cancel", "download", "download-cancel", "batch-cancel", "publish", "subscribe", "port-cancel", "menu", "many", "picker", "upload-hint", "download-hint", "publish-hint", "subscribe-hint", "cancel-hint"} {
		t.Run(scenario, func(t *testing.T) {
			hinted := strings.HasSuffix(scenario, "-hint")
			scenario := strings.TrimSuffix(scenario, "-hint")
			t.Setenv("SSHORE_SOCKET", "")
			t.Setenv("SSH_CONNECTION", "")
			root := t.TempDir()
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(root, "state"))
			source := filepath.Join(root, "tank 中文 with spaces.tar.gz")
			if err := os.WriteFile(source, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			count := 0
			if scenario == "menu" {
				count = 2
			}
			if scenario == "many" {
				count = 100
			}
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("tank extra%03d.txt", i)), []byte("test"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Reaching this local stub proves approval proceeded, without a network call.
			stub := filepath.Join(root, "ssh-stub")
			if err := os.WriteFile(stub, []byte(`#!/bin/sh
printf 'SSH-STARTED\n' >&2
exit 1
`), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSHORE_SSH", stub)
			op := operation{Kind: "upload", Path: "/tmp"}
			if scenario == "download" || scenario == "download-cancel" || scenario == "batch-cancel" {
				op = operation{Kind: "download", Path: "/tmp/new", Name: "new", Size: 4}
			}
			if scenario == "publish" || scenario == "subscribe" || scenario == "port-cancel" {
				command := "publish"
				if scenario == "subscribe" {
					command = "subscribe"
				}
				if command == "publish" {
					op, _ = remoteForwardRequest([]string{"origin:", ".:3000"})
				} else {
					op, _ = remoteForwardRequest([]string{".:3000", "::"})
				}
			}
			if hinted {
				if op.Kind == "forward" {
					op.LocalHint = "8080"
				} else if op.Kind == "upload" {
					op.LocalHint = source
				} else {
					op.LocalHint = filepath.Join(root, "new")
				}
			}
			ops := []operation{op}
			if scenario == "batch-cancel" {
				ops = append(ops, operation{Kind: "download", Path: "/tmp/second", Name: "second", Size: 4})
			}
			startRequestDaemon(t)
			o := testRequestOptions()
			o.host = "example.invalid"
			queued, err := queueRequest(o, ops)
			if err != nil {
				t.Fatal(err)
			}
			id := queued.ID
			args := []string{"review", id}
			if scenario == "publish" || scenario == "subscribe" || scenario == "port-cancel" {
				args = []string{"review", id}
			}
			if scenario == "picker" {
				if _, err := queueRequest(o, []operation{op}); err != nil {
					t.Fatal(err)
				}
				args = []string{"review"}
			}
			cmd := exec.Command(testBinary(t), args...)
			cmd.Env = append(os.Environ(), "TERM=xterm")
			terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 160})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			defer cmd.Process.Kill()
			chunks := make(chan string, 128)
			go func() {
				defer close(chunks)
				b := make([]byte, 4096)
				for {
					n, err := terminal.Read(b)
					if n > 0 {
						chunks <- string(b[:n])
					}
					if err != nil {
						return
					}
				}
			}()
			var output strings.Builder
			seen := 0
			await := func(marker string) {
				t.Helper()
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				for {
					if i := strings.Index(output.String()[seen:], marker); i >= 0 {
						seen += i + len(marker)
						return
					}
					select {
					case s, ok := <-chunks:
						if !ok {
							t.Fatalf("missing %q: %q", marker, output.String())
						}
						output.WriteString(s)
					case <-deadline.C:
						t.Fatalf("timed out waiting for %q: %q", marker, output.String())
					}
				}
			}
			if scenario == "picker" {
				await("Request (#number/name")
				io.WriteString(terminal, "/example.invalid\r"+reviewName(t, id)+"\r")
			}
			if scenario == "publish" || scenario == "subscribe" || scenario == "port-cancel" {
				label := "Local listening port"
				if scenario == "subscribe" {
					label = "Local service port"
				}
				await(label)
				if scenario != "port-cancel" && !hinted {
					io.WriteString(terminal, "\r0\r")
					await("port must be an integer")
				}
				if scenario == "port-cancel" {
					io.WriteString(terminal, "\x03")
					await("approval cancelled")
				} else {
					if hinted {
						io.WriteString(terminal, "\r")
					} else {
						io.WriteString(terminal, "8080\r")
					}
					await("SSH-STARTED")
				}
			} else if scenario == "download" || scenario == "download-cancel" || scenario == "batch-cancel" {
				await("Save as")
				if scenario == "download-cancel" {
					io.WriteString(terminal, "\x03")
					await("approval cancelled")
				} else {
					if hinted {
						io.WriteString(terminal, "\r")
					} else {
						io.WriteString(terminal, "\x15"+filepath.Join(root, "new")+"\r")
					}
					if scenario == "batch-cancel" {
						cwd, _ := os.Getwd()
						await(filepath.Join(cwd, "second"))
						io.WriteString(terminal, "\x15"+filepath.Join(root, "new")+"\r")
						await("destination already selected")
						await("Save as: " + filepath.Join(cwd, "second"))
						io.WriteString(terminal, "\x03")
						await("approval cancelled")
					} else {
						await("SSH-STARTED")
					}
				}
			} else {
				await("Local file to upload")
				if scenario == "cancel" {
					io.WriteString(terminal, "\x03")
					await("approval cancelled")
				} else {
					if scenario == "retry" {
						io.WriteString(terminal, filepath.Join(root, "missing")+"\r")
						await("no such file or directory")
					}
					if hinted {
						await(source)
					} else if scenario == "menu" {
						io.WriteString(terminal, filepath.Join(root, "tank ")+"\t")
						await("Tab: cycle | Enter: select")
						// Select a candidate and accept it; the next Enter submits.
						io.WriteString(terminal, "\t\t\t\r")
					} else if scenario == "many" {
						io.WriteString(terminal, filepath.Join(root, "tank ")+"\t")
						await("101 matches; type more")
						io.WriteString(terminal, "\t\t\t中文\t")
						await("tank 中文 with spaces.tar.gz")
					} else {
						io.WriteString(terminal, filepath.Join(root, "tank")+"\t")
						await("tank 中文 with spaces.tar.gz")
					}
					io.WriteString(terminal, "\r")
					await("SSH-STARTED")
					if scenario == "many" && strings.Count(output.String(), "101 matches;") != 1 {
						t.Fatal("repeated completion hint", output.String())
					}
					if strings.Contains(output.String(), "Type yes to approve:") {
						t.Fatal("upload asked for redundant confirmation")
					}
				}
			}
			_ = cmd.Wait()
			if strings.Contains(output.String(), "Type yes to approve:") {
				t.Fatal("redundant confirmation")
			}
			if strings.Contains(scenario, "cancel") && strings.Contains(output.String(), "SSH-STARTED") {
				t.Fatal("cancelled operation started SSH")
			}
			_, err = findRequest(id)
			if scenario == "publish" || scenario == "subscribe" || scenario == "download" || scenario == "upload" || scenario == "retry" || scenario == "menu" || scenario == "many" || scenario == "picker" {
				if err == nil {
					t.Fatal("approved request not consumed", err)
				}
			} else if err != nil {
				t.Fatal("cancelled request was consumed", err)
			}
		})
	}
}
