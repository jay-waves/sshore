package main

// Session lifecycle fixtures use isolated runtime directories and the CLI test executable.

import (
	"bytes"
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

	"github.com/pkg/sftp"
	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/tmux"

	"github.com/jay-waves/sshore/internal/wire"
)

type testEnv struct {
	t         *testing.T
	bin, dir  string
	env       []string
	processes []*os.Process
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "ss-")
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, bin: testBinary(t), dir: dir}
	state := os.Getenv("SSHORE_STATE_DIR")
	if state == "" {
		state = filepath.Join(dir, "state")
	}
	t.Setenv("SSHORE_STATE_DIR", state)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	e.env = append(os.Environ(), "SSHORE_STATE_DIR="+state, "SSHORE_RUNTIME_DIR="+dir, "GORACE=atexit_sleep_ms=0", "SHELL=/bin/sh", "PS1=sshore-test> ")
	t.Cleanup(func() {
		for _, p := range e.processes {
			_ = p.Kill()
			_ = p.Release()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			snapshot, err := daemonAdmin(ctx, "snapshot", nil)
			if err != nil {
				break
			}
			_, err = daemonAdmin(ctx, "shutdown", &snapshot)
			if err == nil {
				break
			}
			// Test transports may finish between the snapshot and shutdown.
			// Refresh only this isolated fixture; production still requires review.
			if err.Error() == "tasks changed; review again" && ctx.Err() == nil {
				continue
			}
			t.Errorf("test daemon shutdown: %v", err)
			break
		}
		logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
		for _, p := range logs {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("DATA RACE")) {
				t.Errorf("daemon race: %s", b)
			}
		}
		os.RemoveAll(dir)
	})
	return e
}

type bridgeConn struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
	mux *smux.Session
	tty *smux.Stream
}

func (b *bridgeConn) close() {
	if b.mux != nil {
		_ = b.mux.Close()
	}
	_ = b.in.Close()
	_ = b.out.Close()
	_ = b.cmd.Process.Kill()
	_ = b.cmd.Wait()
}
func (e *testEnv) bridge(name string, modes ...string) *bridgeConn {
	e.t.Helper()
	cmd := exec.Command(e.bin, "bridge", name)
	mode := "bridge"
	if len(modes) > 0 {
		mode = modes[0]
	}
	versioned := name == "" || len(modes) > 0
	if versioned {
		cmd = exec.Command(e.bin, "bridge", version, mode, name)
	}
	cmd.Env = e.env
	in, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	if versioned {
		if m := recv(e.t, out); m.Type != "setup_ready" {
			e.t.Fatalf("automatic setup: %+v", m)
		}
	}
	mux, err := clientMux(&wire.Pipe{Reader: out, Writer: in, Closers: []io.Closer{in, out}})
	if err != nil {
		e.t.Fatal(err)
	}
	control, err := mux.OpenStream()
	if err != nil {
		e.t.Fatal(err)
	}
	b := &bridgeConn{cmd: cmd, in: control, out: control, mux: mux}
	e.t.Cleanup(b.close)
	return b
}
func recv(t *testing.T, r io.Reader) message {
	t.Helper()
	type result struct {
		m   message
		err error
	}
	ch := make(chan result, 1)
	go func() { m, err := readMessage(r); ch <- result{m, err} }()
	select {
	case v := <-ch:
		if v.err != nil {
			t.Fatal(v.err)
		}
		return v.m
	case <-time.After(10 * time.Second):
		t.Fatal("protocol response timed out")
	}
	return message{}
}
func sendTest(t *testing.T, w io.Writer, m message) {
	t.Helper()
	if err := writeMessage(w, m); err != nil {
		t.Fatal(err)
	}
}
func (e *testEnv) attach(name string, modes ...string) (*bridgeConn, int) {
	e.t.Helper()
	b := e.bridge(name, modes...)
	action := ""
	if len(modes) > 0 && modes[0] == "attach" {
		action = "existing"
	}
	sendTest(e.t, b.in, message{Type: "attach", Action: action, Cols: 80, Rows: 24, Term: "xterm"})
	m := recv(e.t, b.out)
	if m.Type != "ready" {
		e.t.Fatalf("attach: %+v", m)
	}
	var err error
	b.tty, err = b.mux.AcceptStream()
	if err != nil {
		e.t.Fatal(err)
	}
	if head := recv(e.t, b.tty); head.Type != "terminal" {
		e.t.Fatal(head.Type)
	}
	process, err := os.FindProcess(m.PID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.processes = append(e.processes, process)
	return b, m.PID
}
func untilType(t *testing.T, r io.Reader, kind string) message {
	t.Helper()
	for i := 0; i < 1000; i++ {
		m := recv(t, r)
		if m.Type == "detach_requested" {
			sendTest(t, r.(io.Writer), message{Type: "detach"})
			continue
		}
		if m.Type == kind {
			if m.Type == "detached" {
				sendTest(t, r.(io.Writer), message{Type: "bye"})
			}
			return m
		}
		if m.Type != "output" {
			t.Fatalf("wanted %s, got %+v", kind, m)
		}
	}
	t.Fatal("too many messages")
	return message{}
}
func shellOutput(t *testing.T, b *bridgeConn, command, marker string) string {
	t.Helper()
	sendTest(t, b.tty, message{Type: "input", Data: []byte(command + "\n")})
	var out strings.Builder
	for i := 0; i < 100; i++ {
		m := recv(t, b.tty)
		if m.Type != "output" {
			t.Fatalf("unexpected %+v", m)
		}
		out.Write(m.Data)
		if strings.Contains(out.String(), marker) {
			return out.String()
		}
	}
	t.Fatal("missing output marker")
	return ""
}
func (e *testEnv) helper(action string, paths ...string) (*exec.Cmd, <-chan error) {
	e.t.Helper()
	args := []string{action}
	for _, path := range paths {
		if path != "" {
			args = append(args, path)
		}
	}
	c := exec.Command(e.bin, args...)
	id, err := resolveSession("main")
	if err != nil {
		e.t.Fatal(err)
	}
	c.Env = append(e.env, "SSHORE_SOCKET="+id)
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	e.t.Cleanup(func() { _ = c.Process.Kill() })
	return c, done
}
func waitHelper(t *testing.T, done <-chan error, wantOK bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err == nil) != wantOK {
			t.Fatalf("helper result: %v (want success=%v)", err, wantOK)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper timeout")
	}
}
func TestSessionPersistenceIsolationAndResize(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	// Disable echo so test markers cannot be satisfied by echoed commands.
	shellOutput(t, b, "stty -echo; printf '\\nREADY_%s\\n' yes", "READY_yes")
	output := shellOutput(t, b, "KEEP=shore; printf 'PID_%s\\n' $$", fmt.Sprintf("PID_%d", pid))
	if !strings.Contains(output, fmt.Sprintf("PID_%d", pid)) {
		t.Fatal(output)
	}
	occupied := e.bridge("main")
	sendTest(t, occupied.in, message{Type: "attach", Term: "xterm"})
	if recv(t, occupied.out).Type != "fatal" {
		t.Fatal("accepted second attachment")
	}
	occupied.close()
	sendTest(t, b.in, message{Type: "resize", Cols: 101, Rows: 39})
	untilType(t, b.out, "resized")
	shellOutput(t, b, "stty size; printf 'SIZE_%s\\n' done", "39 101")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()
	b, pid2 := e.attach("main")
	if pid2 != pid {
		t.Fatal("PID changed", pid, pid2)
	}
	shellOutput(t, b, "printf 'KEPT_%s\\n' \"$KEEP\"", "KEPT_shore")
	other, otherPID := e.attach("build")
	if otherPID == pid {
		t.Fatal("named shells share a PID")
	}
	cmd := exec.Command(e.bin, "sessions")
	cmd.Env = e.env
	listing, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(listing, []byte("main")) || !bytes.Contains(listing, []byte("build")) {
		t.Fatalf("listing: %s %v", listing, err)
	}
	sendTest(t, other.tty, message{Type: "input", Data: []byte("exit 7\n")})
	if untilType(t, other.tty, "exit").Code != 7 {
		t.Fatal("lost exit code")
	}
	_, done := e.helper("detach", "")
	untilType(t, b.out, "detached")
	waitHelper(t, done, true)
	b, _ = e.attach("main")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
}
func TestNoClientTransferFails(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	sendTest(t, b.in, message{Type: "detach"})
	untilType(t, b.out, "detached")
	b.close()
	_, done := e.helper("cp", "origin:", ".:"+e.dir)
	waitHelper(t, done, false)
}

// Ensure direct socket queries cannot take over the active attachment.
func TestStatusConnection(t *testing.T) {
	e := newEnv(t)
	_, pid := e.attach("main")
	c, err := e.dial("main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sendTest(t, c, message{Type: "status"})
	m := recv(t, c)
	if m.PID != pid || !m.Attached {
		t.Fatalf("bad status %+v", m)
	}
}

func approvedTestMux(t *testing.T) *smux.Session {
	t.Helper()
	a, b := net.Pipe()
	client, err := smux.Client(a, wire.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, wire.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { serveApprovedStreams(ctx, server); close(done) }()
	t.Cleanup(func() { cancel(); client.Close(); server.Close(); <-done })
	return client
}

func approvedSFTP(t *testing.T, mux *smux.Session, op operation) (*sftp.Client, *smux.Stream) {
	t.Helper()
	st, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	remote := op
	if op.Kind == "upload" {
		remote.Name, remote.Size = "", 0
	}
	sendTest(t, st, message{Type: "approved_transfer", Operations: []operation{remote}, Name: op.Name, Size: op.Size})
	if m := recv(t, st); m.Type != "sftp" {
		t.Fatal(m)
	}
	client, err := files.NewSFTP(st, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); client.Close() })
	return client, st
}

func partialUpload(t *testing.T, mux *smux.Session, dir, name string) *smux.Stream {
	t.Helper()
	client, st := approvedSFTP(t, mux, operation{Kind: "upload", Path: dir, Name: name, Size: 100})
	file, err := client.OpenFile("/.sshore-part", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	return st
}

// Stream closure and remote disk cleanup run concurrently.
func waitUploadFiles(t *testing.T, dir string, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("partial upload files leaked", entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTransferRoundTripsCancellationAndNoOverwrite(t *testing.T) {
	e := newEnv(t)
	mux := approvedTestMux(t)
	data := bytes.Repeat([]byte("transfer\x00\xff"), 200000)
	source := filepath.Join(e.dir, "中文 source.bin")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.dir, "download")
	download := newApprovedFile(operation{Kind: "download", Path: source, Name: filepath.Base(source), Size: int64(len(data))}, target, nil)
	if err := executeApprovedFile(context.Background(), mux, download, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("download corrupted", err)
	}
	dir := filepath.Join(e.dir, "uploads")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	op := newApprovedFile(operation{Kind: "upload", Path: dir}, source, nil)
	approved, err := files.OpenSource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer approved.Close()
	if err = os.Rename(source, source+".original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, make([]byte, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	op.opened = approved
	if err = executeApprovedFile(context.Background(), mux, op, true); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, op.approvedName()))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload corrupted", err)
	}
	upload := func(op fileCopy) error {
		approved, err := files.OpenSource(op.source)
		if err != nil {
			return err
		}
		defer approved.Close()
		op.opened = approved
		return executeApprovedFile(context.Background(), mux, op, true)
	}
	if err = upload(op); err == nil {
		t.Fatal("overwrite accepted")
	}
	st := partialUpload(t, mux, dir, "cancelled")
	st.Close()
	waitUploadFiles(t, dir, 1)
	if err = os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// A different empty source exercises zero-byte publication.
	empty := filepath.Join(t.TempDir(), "empty")
	if err = os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = upload(newApprovedFile(operation{Kind: "upload", Path: e.dir}, empty, nil)); err != nil {
		t.Fatal(err)
	}
	partialUpload(t, mux, dir, "lost")
	mux.Close()
	waitUploadFiles(t, dir, 1)
}

func TestConcurrentStreamsCancelIsolation(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	mux := approvedTestMux(t)
	dir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	slow := partialUpload(t, mux, dir, "cancelled")
	fast, _ := approvedSFTP(t, mux, operation{Kind: "upload", Path: dir, Name: "surviving", Size: 2})
	sendTest(t, b.in, message{Type: "resize", Cols: 97, Rows: 31})
	untilType(t, b.out, "resized")
	shellOutput(t, b, "printf 'LIVE_PID:%s\\n' $$", fmt.Sprintf("LIVE_PID:%d", pid))
	slow.Close()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := files.OpenSource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := opened.Upload(fast, "/target", nil); err != nil {
		t.Fatal(err)
	}
	waitUploadFiles(t, dir, 1)
	shellOutput(t, b, "printf 'AFTER_%s\\n' cancel", "AFTER_cancel")
}

func TestDisconnectCleansBothConcurrentUploads(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	mux := approvedTestMux(t)
	dir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		partialUpload(t, mux, dir, fmt.Sprintf("file%d", i))
	}
	mux.Close()
	waitUploadFiles(t, dir, 0)
	b.close()
	resumed, resumedPID := e.attach("main")
	if resumedPID != pid {
		t.Fatal("upload disconnect replaced the shell")
	}
	shellOutput(t, resumed, "printf CLEAN_%s alive", "CLEAN_alive")
}

// Automatic connections exercise versioned startup too: an empty name must survive its
// validation, and named reconnect must retain the original shell.
func TestAutomaticSessionsAndNamedResume(t *testing.T) {
	e := newEnv(t)
	first, pid := e.attach("")
	sendTest(t, first.in, message{Type: "session_targets", Action: "list"})
	firstName := untilType(t, first.out, "session_targets").Name
	second, otherPID := e.attach("")
	if pid == otherPID {
		t.Fatal("automatic connections reused one shell")
	}
	sendTest(t, first.in, message{Type: "detach"})
	untilType(t, first.out, "detached")
	first.close()
	third, thirdPID := e.attach("")
	if thirdPID == pid || thirdPID == otherPID {
		t.Fatal("automatic connection reused an occupied name")
	}
	resumed, resumedPID := e.attach(firstName)
	if resumedPID != pid {
		t.Fatal("named reconnect did not resume the first automatic shell")
	}
	for _, b := range []*bridgeConn{resumed, second, third} {
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
		b.close()
	}
}

// Losing a client preserves both the shell and its foreground job.
func TestDisconnectPreservesSession(t *testing.T) {
	for _, foreground := range []bool{false, true} {
		t.Run(fmt.Sprint(foreground), func(t *testing.T) {
			e := newEnv(t)
			b, pid := e.attach("lost")
			if foreground {
				shellOutput(t, b, "stty -echo; sh -c 'printf JOB_%s ready; exec sleep 300'", "JOB_ready")
			}
			b.close()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				c, err := e.dial("lost")
				if err == nil {
					_ = c.SetDeadline(time.Now().Add(time.Second))
					_ = writeMessage(c, message{Type: "status"})
					m, err := readMessage(c)
					c.Close()
					if err == nil && m.PID == pid && !m.Attached {
						return
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("shell did not survive client loss as detached")
		})
	}
}

func waitSessionEnded(t *testing.T, e *testEnv, name string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id, lookupErr := resolveSession(name)
		stat, processErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// An orphan awaiting reaping is no longer a running shell.
		dead := os.IsNotExist(processErr) || strings.Contains(string(stat), ") Z ")
		if lookupErr == nil && id == "" && dead {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell %s (shell %d) survived connection loss", name, pid)
}

func TestExplicitDetachControlPreservesSession(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("stay")
	if err := tmux.Detach(b.in.(*smux.Stream)); err != nil {
		t.Fatal(err)
	}
	b.close()
	next, resumedPID := e.attach("stay")
	if resumedPID != pid {
		t.Fatal("explicit detach replaced shell")
	}
	sendTest(t, next.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, next.tty, "exit")
	next.close()
	waitSessionEnded(t, e, "stay", pid)
}

// Public creation and attachment use distinct startup modes. All subprocesses
// come from testBinary (isolated CLI test executable) and use newEnv's isolated runtime.
func TestNewAndAttachLifecycle(t *testing.T) {
	e := newEnv(t)
	expectFailure := func(mode, name, contains string) {
		t.Helper()
		cmd := exec.Command(e.bin, "bridge", version, mode, name)
		cmd.Env = e.env
		data, err := cmd.Output()
		if err == nil {
			t.Fatalf("%s %s succeeded unexpectedly", mode, name)
		}
		m, err := readMessage(bytes.NewReader(data))
		if err != nil || m.Type != "fatal" || !strings.Contains(m.Error, contains) {
			t.Fatalf("%s %s: %+v %v", mode, name, m, err)
		}
	}
	expectFailure("attach", "missing", "does not exist")
	expectFailure("attach", "", "no detached shells")
	first, pid := e.attach("build", "new")
	expectFailure("new", "build", "already exists")
	expectFailure("attach", "", "no detached shells")
	sendTest(t, first.in, message{Type: "detach"})
	untilType(t, first.out, "detached")
	first.close()
	expectFailure("new", "build", "already exists")
	resumed, resumedPID := e.attach("", "attach")
	if resumedPID != pid {
		t.Fatal("attach created another shell")
	}
	sendTest(t, resumed.in, message{Type: "detach"})
	untilType(t, resumed.out, "detached")
	resumed.close()
	second, _ := e.attach("other", "new")
	sendTest(t, second.in, message{Type: "detach"})
	untilType(t, second.out, "detached")
	second.close()
	// Observe the real multi-session offer, then cancel by closing the transport.
	cmd := exec.Command(e.bin, "bridge", version, "attach", "")
	cmd.Env = e.env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	m := recv(t, out)
	names, err := sessionTargetNames(m.Targets)
	if m.Type != "session_choice" || err != nil || strings.Join(names, ",") != "build,other" {
		t.Fatalf("choice: %+v", m)
	}
	in.Close()
	cmd.Wait()
	out.Close()
	for _, name := range []string{"build", "other"} {
		b, gotPID := e.attach(name, "attach")
		if name == "build" && gotPID != pid {
			t.Fatal("selection cancellation lost shell")
		}
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
		b.close()
	}
}

func TestSharedRemoteDaemonEnvironment(t *testing.T) {
	e := newEnv(t)
	e.env = append(e.env, "SSHORE_TEST_VALUE=first")
	first, firstPID := e.attach("envfirst")
	e.env = append(e.env, "SSHORE_TEST_VALUE=second")
	second, secondPID := e.attach("envsecond")
	a, err := readSetupTask(firstPID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := readSetupTask(secondPID)
	if err != nil || a.parent != b.parent {
		t.Fatalf("separate daemons: %+v %+v %v", a, b, err)
	}
	shellOutput(t, first, "printf 'ENV_%s_VALUE' \"$SSHORE_TEST_VALUE\"", "ENV_first_VALUE")
	shellOutput(t, second, "printf 'ENV_%s_VALUE' \"$SSHORE_TEST_VALUE\"", "ENV_second_VALUE")
}

func (e *testEnv) dial(name string) (net.Conn, error) {
	id, err := resolveSession(name)
	if err != nil {
		return nil, err
	}
	return dialHostedTask(context.Background(), id, time.Second)
}

// Close only the authenticated daemon under this test fixture state root.
func shutdownFixtureDaemon(t *testing.T, root string) {
	t.Helper()
	// This fixture gives the frontend its own account state root. Shut that
	// daemon down before newEnv removes its endpoint and owner records.
	original, had := os.LookupEnv("SSHORE_STATE_DIR")
	os.Setenv("SSHORE_STATE_DIR", root)
	defer func() {
		if had {
			os.Setenv("SSHORE_STATE_DIR", original)
		} else {
			os.Unsetenv("SSHORE_STATE_DIR")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		snapshot, err := daemonAdmin(ctx, "snapshot", nil)
		if err != nil {
			return
		}
		if _, err := daemonAdmin(ctx, "shutdown", &snapshot); err == nil {
			return
		} else if err.Error() != "tasks changed; review again" || ctx.Err() != nil {
			t.Errorf("frontend fixture shutdown: %v", err)
			return
		}
	}
}

func TestAttachmentRejectsTunnelStreams(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveApprovedStreams(ctx, server)
	for _, kind := range []string{"forward_connect", "forward_control"} {
		st, e := client.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		st.SetDeadline(time.Now().Add(time.Second))
		if e = writeMessage(st, message{Type: kind, Action: "add"}); e != nil {
			t.Fatal(e)
		}
		if m, e := readMessage(st); e == nil {
			t.Fatalf("obsolete attachment tunnel accepted: %+v", m)
		}
		st.Close()
	}
}
