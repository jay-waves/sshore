package main

// Run repository checks with go test -race ./... and go vet ./.... These Linux
// integration tests build a race-enabled binary and exercise real session
// processes: isolation, exclusive attachment, resizing, exit codes, and preserved
// PID/environment after explicit detach, and teardown on disconnection. Transfer cases cover both
// directions through approved streams, unread-stream isolation, independent
// cancellation, and cleanup after disconnection. Framing, integrity, overwrite
// protection, and client worker concurrency also have focused unit tests.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
func (e *testEnv) bridge(name string) *bridgeConn {
	e.t.Helper()
	cmd := exec.Command(e.bin, "bridge", name)
	if name == "" {
		cmd = exec.Command(e.bin, "bootstrap", e.bin, "bridge", "")
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
	if name == "" {
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
func (e *testEnv) attach(name string) (*bridgeConn, int) {
	e.t.Helper()
	b := e.bridge(name)
	sendTest(e.t, b.in, message{Type: "attach", Cols: 80, Rows: 24, Term: "xterm"})
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
func (e *testEnv) helper(action, path string) (*exec.Cmd, <-chan error) {
	e.t.Helper()
	args := []string{action}
	if path != "" {
		args = append(args, path)
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
	_, done := e.helper("upload", e.dir)
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

func approvedStream(t *testing.T, b *bridgeConn, op operation) (*smux.Stream, message) {
	t.Helper()
	st, err := b.mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sendTest(t, st, message{Type: "approved_transfer", Operations: []operation{op}})
	return st, recv(t, st)
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

func uploadBytes(t *testing.T, st *smux.Stream, id uint64, name string, data []byte) {
	t.Helper()
	sendTest(t, st, message{Type: "offer", ID: id, Name: name, Size: int64(len(data))})
	untilType(t, st, "accept")
	for off := 0; off < len(data); off += chunkSize {
		sendTest(t, st, message{Type: "chunk", ID: id, Data: data[off:min(off+chunkSize, len(data))]})
	}
	sum := sha256.Sum256(data)
	sendTest(t, st, message{Type: "end", ID: id, Hash: hex.EncodeToString(sum[:])})
	if m := untilType(t, st, "result"); m.Error != "" {
		t.Fatal(m.Error)
	}
}
func TestTransferRoundTripsCancellationAndNoOverwrite(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	data := make([]byte, 2*1024*1024+17)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := filepath.Join(e.dir, "\u4e2d\u6587 sample.bin")
	if err := os.WriteFile(src, data, 0600); err != nil {
		t.Fatal(err)
	}
	st, offer := approvedStream(t, b, operation{Kind: "download", Path: src, Name: filepath.Base(src), Size: int64(len(data))})
	sendTest(t, st, message{Type: "accept", ID: offer.ID})
	var received []byte
	for {
		m := recv(t, st)
		if m.Type == "end" {
			sum := sha256.Sum256(received)
			if m.Hash != hex.EncodeToString(sum[:]) {
				t.Fatal("download checksum")
			}
			break
		}
		if m.Type != "chunk" {
			t.Fatalf("download: %+v", m)
		}
		received = append(received, m.Data...)
	}
	if !bytes.Equal(received, data) {
		t.Fatal("download corrupted")
	}
	sendTest(t, st, message{Type: "result", ID: offer.ID})
	_ = st.Close()
	uploadDir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(uploadDir, 0700); err != nil {
		t.Fatal(err)
	}
	st, request := approvedStream(t, b, operation{Kind: "upload", Path: uploadDir})
	uploadBytes(t, st, request.ID, "received.bin", data)
	_ = st.Close()
	got, err := os.ReadFile(filepath.Join(uploadDir, "received.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload corrupted", err)
	}
	st, request = approvedStream(t, b, operation{Kind: "upload", Path: uploadDir})
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "received.bin", Size: 0})
	untilType(t, st, "error")
	_ = st.Close()
	st, request = approvedStream(t, b, operation{Kind: "upload", Path: uploadDir})
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "cancelled.bin", Size: 100})
	untilType(t, st, "accept")
	sendTest(t, st, message{Type: "chunk", ID: request.ID, Data: []byte("partial")})
	_ = st.Close()
	waitUploadFiles(t, uploadDir, 1)
	st, request = approvedStream(t, b, operation{Kind: "upload", Path: uploadDir})
	uploadBytes(t, st, request.ID, "empty", nil)
	_ = st.Close()
	st, request = approvedStream(t, b, operation{Kind: "upload", Path: uploadDir})
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "lost", Size: 100})
	untilType(t, st, "accept")
	b.close()
	waitUploadFiles(t, uploadDir, 2)
}

func TestConcurrentStreamsBackpressureAndCancelIsolation(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	shellOutput(t, b, "stty -echo; printf 'READY_%s\\n' mux", "READY_mux")
	data := bytes.Repeat([]byte("large download data\x00\xff"), 1024*1024)
	src := filepath.Join(e.dir, "large.bin")
	if err := os.WriteFile(src, data, 0600); err != nil {
		t.Fatal(err)
	}
	slow, offer := approvedStream(t, b, operation{Kind: "download", Path: src, Name: filepath.Base(src), Size: int64(len(data))})
	sendTest(t, slow, message{Type: "accept", ID: offer.ID})
	// Deliberately stop reading the download until its stream window fills.
	// Other streams must remain usable on this very same SSH transport.
	time.Sleep(100 * time.Millisecond)
	fast, request := approvedStream(t, b, operation{Kind: "upload", Path: e.dir})
	sendTest(t, b.in, message{Type: "resize", Cols: 97, Rows: 31})
	untilType(t, b.out, "resized")
	started := time.Now()
	shellOutput(t, b, "printf 'LIVE_PID:%s\\n' $$", fmt.Sprintf("LIVE_PID:%d", pid))
	if time.Since(started) > 2*time.Second {
		t.Fatal("terminal stalled behind download")
	}
	uploadBytes(t, fast, request.ID, "concurrent.bin", []byte("upload while download is blocked"))
	_ = fast.Close()
	// Cancelling one stream must leave the session and a sibling upload alive.
	fast, request = approvedStream(t, b, operation{Kind: "upload", Path: e.dir})
	sendTest(t, fast, message{Type: "offer", ID: request.ID, Name: "surviving.bin", Size: 2})
	untilType(t, fast, "accept")
	_ = slow.Close()
	sendTest(t, fast, message{Type: "chunk", ID: request.ID, Data: []byte("ok")})
	sum := sha256.Sum256([]byte("ok"))
	sendTest(t, fast, message{Type: "end", ID: request.ID, Hash: hex.EncodeToString(sum[:])})
	untilType(t, fast, "result")
	shellOutput(t, b, "printf 'AFTER_%s\\n' cancel", "AFTER_cancel")
}

func TestDisconnectCleansBothConcurrentUploads(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	dir := filepath.Join(e.dir, "uploads")
	_ = os.Mkdir(dir, 0700)
	for i := 0; i < 2; i++ {
		st, m := approvedStream(t, b, operation{Kind: "upload", Path: dir})
		sendTest(t, st, message{Type: "offer", ID: m.ID, Name: fmt.Sprintf("file%d", i), Size: 100})
		untilType(t, st, "accept")
		sendTest(t, st, message{Type: "chunk", ID: m.ID, Data: []byte("partial")})
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
