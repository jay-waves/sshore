package main

// Session lifecycle fixtures use isolated runtime directories and tagged binaries.

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pkg/sftp"
	"github.com/xtaci/smux"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testBuild struct {
	sync.Once
	path string
	err  error
}

func testBinary(t *testing.T) string {
	t.Helper()
	testBuild.Do(func() {
		dir, err := os.MkdirTemp("", "sshore-bin-")
		if err != nil {
			testBuild.err = err
			return
		}
		testBuild.path = filepath.Join(dir, "sshore")
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-tags=sshoretest", "-buildvcs=false", "-race", "-o", testBuild.path, ".")
		b, err := cmd.CombinedOutput()
		if err != nil {
			testBuild.err = fmt.Errorf("build: %w: %s", err, b)
		}
	})
	if testBuild.err != nil {
		t.Fatal(testBuild.err)
	}
	return testBuild.path
}
func TestMain(m *testing.M) {
	code := m.Run()
	if testBuild.path != "" {
		os.RemoveAll(filepath.Dir(testBuild.path))
	}
	os.Exit(code)
}

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
	e.env = append(os.Environ(), "SSHORE_RUNTIME_DIR="+dir, "GORACE=atexit_sleep_ms=0", "SHELL=/bin/sh", "PS1=sshore-test> ")
	t.Cleanup(func() {
		for _, p := range e.processes {
			_ = p.Kill()
			_ = p.Release()
		}
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			paths, _ := filepath.Glob(filepath.Join(dir, "*.sock"))
			if len(paths) == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if paths, _ := filepath.Glob(filepath.Join(dir, "*.sock")); len(paths) != 0 {
			t.Errorf("test session failed to exit: %v", paths)
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
	bootstrap := name == "" || len(modes) > 0
	if bootstrap {
		cmd = exec.Command(e.bin, "bootstrap", e.bin, mode, name)
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
	if bootstrap {
		if m := recv(e.t, out); m.Type != "setup_ready" {
			e.t.Fatalf("automatic setup: %+v", m)
		}
	}
	mux, err := clientMux(&pipeTransport{Reader: out, Writer: in, closers: []io.Closer{in, out}})
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
	c.Env = append(e.env, "SSHORE_SOCKET="+filepath.Join(e.dir, "main.sock"))
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
		t.Fatal("named sessions share a PID")
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
	_, done := e.helper("cp", "origin:", e.dir)
	waitHelper(t, done, false)
}

// Ensure direct socket queries cannot take over the active attachment.
func TestStatusConnection(t *testing.T) {
	e := newEnv(t)
	_, pid := e.attach("main")
	c, err := net.Dial("unix", filepath.Join(e.dir, "main.sock"))
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

func approvedSFTP(t *testing.T, b *bridgeConn, op operation) (*sftp.Client, *smux.Stream) {
	t.Helper()
	st, err := b.mux.OpenStream()
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
	client, err := newFTPSFTP(st, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); client.Close() })
	return client, st
}

func partialUpload(t *testing.T, b *bridgeConn, dir, name string) *smux.Stream {
	t.Helper()
	client, st := approvedSFTP(t, b, operation{Kind: "upload", Path: dir, Name: name, Size: 100})
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
	b, _ := e.attach("main")
	data := bytes.Repeat([]byte("transfer\x00\xff"), 200000)
	source := filepath.Join(e.dir, "中文 source.bin")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.dir, "download")
	op := operation{Kind: "download", Path: source, Name: filepath.Base(source), Size: int64(len(data)), LocalPath: target}
	if err := executeApprovedFile(context.Background(), b.mux, op, true); err != nil {
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
	op = operation{Kind: "upload", Path: dir, Name: filepath.Base(source), Size: int64(len(data)), LocalPath: source}
	if err = executeApprovedFile(context.Background(), b.mux, op, true); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, op.Name))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload corrupted", err)
	}
	if err = executeApprovedFile(context.Background(), b.mux, op, true); err == nil {
		t.Fatal("overwrite accepted")
	}
	st := partialUpload(t, b, dir, "cancelled")
	st.Close()
	waitUploadFiles(t, dir, 1)
	if err = os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	op.Size = 0
	op.Path = e.dir
	// A different empty source exercises zero-byte publication.
	op.LocalPath = filepath.Join(t.TempDir(), "empty")
	op.Name = "empty"
	if err = os.WriteFile(op.LocalPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = executeApprovedFile(context.Background(), b.mux, op, true); err != nil {
		t.Fatal(err)
	}
	partialUpload(t, b, dir, "lost")
	b.close()
	waitUploadFiles(t, dir, 1)
}

func TestConcurrentStreamsCancelIsolation(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	dir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	slow := partialUpload(t, b, dir, "cancelled")
	fast, _ := approvedSFTP(t, b, operation{Kind: "upload", Path: dir, Name: "surviving", Size: 2})
	sendTest(t, b.in, message{Type: "resize", Cols: 97, Rows: 31})
	untilType(t, b.out, "resized")
	shellOutput(t, b, "printf 'LIVE_PID:%s\\n' $$", fmt.Sprintf("LIVE_PID:%d", pid))
	slow.Close()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyUpload(fast, source, "/target", 2, nil); err != nil {
		t.Fatal(err)
	}
	waitUploadFiles(t, dir, 1)
	shellOutput(t, b, "printf 'AFTER_%s\\n' cancel", "AFTER_cancel")
}

func TestDisconnectCleansBothConcurrentUploads(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	dir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		partialUpload(t, b, dir, fmt.Sprintf("file%d", i))
	}
	b.close()
	waitUploadFiles(t, dir, 0)
	waitSessionEnded(t, e, "main", pid)
}

// Automatic connections exercise bootstrap too: an empty name must survive its
// validation, and named reconnect must retain the original shell.
func TestAutomaticSessionsAndNamedResume(t *testing.T) {
	e := newEnv(t)
	first, pid := e.attach("")
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
	resumed, resumedPID := e.attach("pikachu")
	if resumedPID != pid {
		t.Fatal("named reconnect did not resume the first automatic session")
	}
	for _, b := range []*bridgeConn{resumed, second, third} {
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
		b.close()
	}
}

// A closed transport must hang up the PTY, not silently become a detach.
func TestDisconnectEndsSession(t *testing.T) {
	for _, foreground := range []bool{false, true} {
		t.Run(fmt.Sprint(foreground), func(t *testing.T) {
			e := newEnv(t)
			b, pid := e.attach("lost")
			if foreground {
				shellOutput(t, b, "stty -echo; sh -c 'printf JOB_%s ready; exec sleep 300'", "JOB_ready")
			}
			b.close()
			waitSessionEnded(t, e, "lost", pid)
		})
	}
}

func waitSessionEnded(t *testing.T, e *testEnv, name string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, socketErr := os.Stat(filepath.Join(e.dir, name+".sock"))
		stat, processErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// An orphan awaiting reaping is no longer a running shell.
		dead := os.IsNotExist(processErr) || strings.Contains(string(stat), ") Z ")
		if os.IsNotExist(socketErr) && dead {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s (shell %d) survived connection loss", name, pid)
}

func TestExplicitDetachControlPreservesSession(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("stay")
	if err := detachControl(b.in.(*smux.Stream)); err != nil {
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

// Public creation and attachment use distinct bootstrap modes. All subprocesses
// come from testBinary (-tags=sshoretest) and use newEnv's isolated runtime.
func TestNewAndAttachLifecycle(t *testing.T) {
	e := newEnv(t)
	expectFailure := func(mode, name, contains string) {
		t.Helper()
		cmd := exec.Command(e.bin, "bootstrap", e.bin, mode, name)
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
	expectFailure("attach", "", "no detached sessions")
	first, pid := e.attach("build", "new")
	expectFailure("new", "build", "already exists")
	expectFailure("attach", "", "no detached sessions")
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
	cmd := exec.Command(e.bin, "bootstrap", e.bin, "attach", "")
	cmd.Env = e.env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	m := recv(t, out)
	if m.Type != "session_choice" || strings.Join(m.Names, ",") != "build,other" {
		t.Fatalf("choice: %+v", m)
	}
	in.Close()
	cmd.Wait()
	out.Close()
	for _, name := range []string{"build", "other"} {
		b, gotPID := e.attach(name, "attach")
		if name == "build" && gotPID != pid {
			t.Fatal("selection cancellation lost session")
		}
		sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
		untilType(t, b.tty, "exit")
		b.close()
	}
}
