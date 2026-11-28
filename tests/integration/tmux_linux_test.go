//go:build integration && linux

package tests

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/tmux"
)

func tmuxTestCgroup(t *testing.T) string {
	t.Helper()
	if os.Getenv("SSHORE_TEST_CGROUP") == "" {
		t.Skip("run in the disposable Alpine fixture with a delegated cgroup")
	}
	cgroup, err := sys.DaemonTaskCgroup()
	if err != nil {
		t.Fatal(err)
	}
	return cgroup
}

func TestTmuxSession(t *testing.T) {
	cgroup := tmuxTestCgroup(t)
	var manager tmux.Manager
	changed := make(chan struct{}, 1)
	events := tmux.Events{Changed: func(...string) {
		select {
		case changed <- struct{}{}:
		default:
		}
	}}
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	config := tmux.ProcessConfig{
		Directory: home, TaskCgroup: cgroup,
		Environment: []string{"HOME=" + home, "SHELL=/bin/sh", "PATH=/usr/bin:/bin"},
	}
	create := func(name string, parent *tmux.Session) *tmux.Session {
		t.Helper()
		session, err := manager.Create(tmux.SessionConfig{ID: name, Name: name, Process: config, Parent: parent, Events: events})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(session.Close)
		return session
	}
	type frontend struct {
		conn   net.Conn
		lines  *bufio.Scanner
		stream *tmux.Attachment
		state  tmux.Snapshot
		key    string
	}
	attach := func(session *tmux.Session, key string, startup tmux.Startup, existing bool) frontend {
		t.Helper()
		client, terminal := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		p := frontend{conn: client, lines: bufio.NewScanner(client), stream: tmux.NewAttachment(terminal)}
		t.Cleanup(func() { _, _ = p.stream.Disconnect() })
		err := session.Attach(p.stream, tmux.AttachOptions{
			TreeKey: key, Size: pty.Size{Cols: 80, Rows: 24}, Term: "xterm", Startup: startup, Existing: existing,
		}, func(key string, state tmux.Snapshot) error {
			p.key, p.state = key, state
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	expectState := func(p frontend, command, value string, pid int) {
		t.Helper()
		_ = p.conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintln(p.conn, command); err != nil {
			t.Fatal(err)
		}
		want := "SESSION_STATE=" + strconv.Itoa(pid) + ":" + value
		for p.lines.Scan() {
			if strings.TrimSpace(p.lines.Text()) == want {
				return
			}
		}
		t.Fatalf("process did not report %q: %v", want, p.lines.Err())
	}
	disconnect := func(p frontend) {
		t.Helper()
		if detached, err := p.stream.Disconnect(); err != nil || !detached.Released {
			t.Fatalf("disconnect: %+v, %v", detached, err)
		}
	}

	parent := create("parent", nil)
	root := attach(parent, "", tmux.Startup{NoProfile: true}, false)
	const report = `printf '\nSESSION_STATE=%s:%s\n' "$$" "$value"`
	expectState(root, "cd work; value=parent; "+report, "parent", root.state.PID)

	// Entering a child suspends the parent's terminal without ending its shell.
	release, ok := parent.Hold(root.key)
	if !ok {
		t.Fatal("could not reserve the parent's terminal")
	}
	defer release()
	disconnect(root)
	child := create("child", parent)
	program := `value=child; while IFS= read -r request; do ` + report + `; done`
	first := attach(child, root.key, tmux.Startup{Argv: []string{"/bin/sh", "-c", program}}, false)
	expectState(first, "report", "child", first.state.PID)
	disconnect(first)

	resumed := attach(child, root.key, tmux.Startup{}, true)
	expectState(resumed, "report", "child", first.state.PID)
	// Losing the data channel releases input even with no control protocol.
	if err := resumed.conn.Close(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for child.Snapshot().Active != nil {
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("terminal EOF left the child's input reserved")
		}
	}
	returned := attach(parent, root.key, tmux.Startup{}, true)
	expectState(returned, report, "parent", root.state.PID)
	if directory := parent.WorkingDirectory(); directory != work {
		t.Fatalf("resumed directory = %q, want %q", directory, work)
	}
	if manager.Lookup("child") != child {
		t.Fatal("disconnect removed the live child")
	}
}

type observedTerminalWrite struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (s *observedTerminalWrite) Write(data []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	return s.Conn.Write(data)
}

func TestTmuxSlowAttach(t *testing.T) {
	cgroup := tmuxTestCgroup(t)
	for _, phase := range []string{"ready", "replay"} {
		t.Run(phase, func(t *testing.T) {
			var manager tmux.Manager
			session, err := manager.Create(tmux.SessionConfig{ID: phase, Process: tmux.ProcessConfig{
				Directory: t.TempDir(), TaskCgroup: cgroup,
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(session.Close)
			pipe := func() (net.Conn, net.Conn) {
				client, server := net.Pipe()
				t.Cleanup(func() {
					_ = client.Close()
					_ = server.Close()
				})
				return client, server
			}
			const mode = "\x1b[?2004h"
			opts := tmux.AttachOptions{Term: "xterm", Size: pty.Size{Cols: 80, Rows: 24},
				Startup: tmux.Startup{Argv: []string{"/bin/sh", "-c", `printf '\033[?2004h'; read -r line`}}}
			if phase == "replay" {
				client, stream := pipe()
				front := tmux.NewAttachment(stream)
				if err := session.Attach(front, opts, nil); err != nil {
					t.Fatal(err)
				}
				_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, len(mode))
				if _, err := io.ReadFull(client, buf); err != nil || string(buf) != mode {
					t.Fatalf("could not seed the terminal mode to replay: %q, %v", buf, err)
				}
				if _, err := front.Disconnect(); err != nil {
					t.Fatal(err)
				}
				opts.Existing, opts.Startup = true, tmux.Startup{}
			}
			_, data := pipe()
			started := make(chan struct{})
			var ready func(string, tmux.Snapshot) error
			if phase == "replay" {
				data = &observedTerminalWrite{Conn: data, started: started}
			}
			front := tmux.NewAttachment(data)
			if phase == "ready" {
				_, control := pipe()
				go func() {
					select {
					case <-front.Done():
					case <-t.Context().Done():
					}
					_ = control.Close()
				}()
				ready = func(string, tmux.Snapshot) error {
					close(started)
					_, err := io.WriteString(control, "ready")
					return err
				}
			}
			attached := make(chan error, 1)
			go func() { attached <- session.Attach(front, opts, ready) }()
			select {
			case <-started:
			case err := <-attached:
				t.Fatalf("attachment never wrote %s: %v", phase, err)
			case <-time.After(5 * time.Second):
				t.Fatalf("attachment never wrote %s", phase)
			}

			// Leave the frontend unreadable. Both operations must work before the
			// terminal's write deadline, and Stop must cancel the pending write.
			states := make(chan tmux.Snapshot, 1)
			go func() { states <- session.Snapshot() }()
			select {
			case snapshot := <-states:
				if !snapshot.Attaching || snapshot.Active != nil || snapshot.PID <= 0 {
					t.Fatalf("expected a pending attachment, got %+v", snapshot)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("slow attachment blocked the session query")
			}
			stopped := make(chan error, 1)
			go func() { stopped <- session.Stop("test stopped session") }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("slow attachment blocked session stop")
			}
			select {
			case err := <-attached:
				if err == nil {
					t.Fatal("stopped session accepted the attachment")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stopping did not cancel the pending attachment")
			}
		})
	}
}
