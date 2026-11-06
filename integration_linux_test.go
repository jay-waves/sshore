package main

// Run repository checks with go test -race ./... and go vet ./.... These Linux
// integration tests build a race-enabled binary and exercise real session
// processes: isolation, exclusive attachment, resizing, exit codes, and preserved
// PID/environment after detach or disconnection. Transfer cases cover both
// directions, the two-task limit, unread-stream isolation, independent
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
	// Low-level transfer regression tests run the wire helpers directly; the
	// public CLI now queues approval instead of initiating these transfers.
	if os.Getenv("SSHORE_TEST_WIRE_HELPER") == "1" {
		args := os.Args[1:]
		var err error
		if args[0] == "download" && len(args) > 2 {
			err = downloadFiles(args[1:])
		} else {
			path := ""
			if len(args) > 1 {
				path = args[1]
			}
			err = helper(args[0], path)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

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
	c := exec.Command(os.Args[0], args...)
	c.Env = append(e.env, "SSHORE_TEST_WIRE_HELPER=1", "SSHORE_SOCKET="+filepath.Join(e.dir, "main.sock"))
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
	b.close() // Abrupt transport loss, no detach message.
	time.Sleep(100 * time.Millisecond)
	b, pid2 = e.attach("main")
	if pid2 != pid {
		t.Fatal("PID changed after EOF")
	}
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

func acceptFile(t *testing.T, b *bridgeConn) (*smux.Stream, message) {
	t.Helper()
	_ = b.mux.SetDeadline(time.Now().Add(5 * time.Second))
	st, err := b.mux.AcceptStream()
	_ = b.mux.SetDeadline(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, recv(t, st)
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
	_, done := e.helper("download", src)
	st, offer := acceptFile(t, b)
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
	waitHelper(t, done, true)
	_ = st.Close()
	uploadDir := filepath.Join(e.dir, "uploads")
	if err := os.Mkdir(uploadDir, 0700); err != nil {
		t.Fatal(err)
	}
	_, done = e.helper("upload", uploadDir)
	st, request := acceptFile(t, b)
	uploadBytes(t, st, request.ID, "received.bin", data)
	waitHelper(t, done, true)
	_ = st.Close()
	got, err := os.ReadFile(filepath.Join(uploadDir, "received.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload corrupted", err)
	}
	_, done = e.helper("upload", uploadDir)
	st, request = acceptFile(t, b)
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "received.bin", Size: 0})
	untilType(t, st, "error")
	waitHelper(t, done, false)
	_ = st.Close()
	_, done = e.helper("upload", uploadDir)
	st, request = acceptFile(t, b)
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "cancelled.bin", Size: 100})
	untilType(t, st, "accept")
	sendTest(t, st, message{Type: "chunk", ID: request.ID, Data: []byte("partial")})
	_ = st.Close()
	waitHelper(t, done, false)
	entries, _ := os.ReadDir(uploadDir)
	if len(entries) != 1 {
		t.Fatal("cancel leaked partial file", entries)
	}
	_, done = e.helper("upload", uploadDir)
	st, request = acceptFile(t, b)
	uploadBytes(t, st, request.ID, "empty", nil)
	waitHelper(t, done, true)
	_ = st.Close()
	_, done = e.helper("upload", uploadDir)
	st, request = acceptFile(t, b)
	sendTest(t, st, message{Type: "offer", ID: request.ID, Name: "lost", Size: 100})
	untilType(t, st, "accept")
	b.close()
	waitHelper(t, done, false)
	entries, _ = os.ReadDir(uploadDir)
	if len(entries) != 2 {
		t.Fatal("disconnect leaked partial file", entries)
	}
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
	_, downloadDone := e.helper("download", src)
	slow, offer := acceptFile(t, b)
	sendTest(t, slow, message{Type: "accept", ID: offer.ID})
	// Deliberately stop reading the download until its stream window fills.
	// Other streams must remain usable on this very same SSH transport.
	time.Sleep(100 * time.Millisecond)
	_, uploadDone := e.helper("upload", e.dir)
	fast, request := acceptFile(t, b)
	_, rejected := e.helper("upload", e.dir)
	waitHelper(t, rejected, false)
	sendTest(t, b.in, message{Type: "resize", Cols: 97, Rows: 31})
	untilType(t, b.out, "resized")
	started := time.Now()
	shellOutput(t, b, "printf 'LIVE_PID:%s\\n' $$", fmt.Sprintf("LIVE_PID:%d", pid))
	if time.Since(started) > 2*time.Second {
		t.Fatal("terminal stalled behind download")
	}
	uploadBytes(t, fast, request.ID, "concurrent.bin", []byte("upload while download is blocked"))
	waitHelper(t, uploadDone, true)
	_ = fast.Close()
	// Cancelling one stream must leave the session and a sibling upload alive.
	_, uploadDone = e.helper("upload", e.dir)
	fast, request = acceptFile(t, b)
	sendTest(t, fast, message{Type: "offer", ID: request.ID, Name: "surviving.bin", Size: 2})
	untilType(t, fast, "accept")
	_ = slow.Close()
	waitHelper(t, downloadDone, false)
	sendTest(t, fast, message{Type: "chunk", ID: request.ID, Data: []byte("ok")})
	sum := sha256.Sum256([]byte("ok"))
	sendTest(t, fast, message{Type: "end", ID: request.ID, Hash: hex.EncodeToString(sum[:])})
	untilType(t, fast, "result")
	waitHelper(t, uploadDone, true)
	shellOutput(t, b, "printf 'AFTER_%s\\n' cancel", "AFTER_cancel")
}

func TestDisconnectCleansBothConcurrentUploads(t *testing.T) {
	e := newEnv(t)
	b, pid := e.attach("main")
	dir := filepath.Join(e.dir, "uploads")
	_ = os.Mkdir(dir, 0700)
	done := make([]<-chan error, 2)
	for i := 0; i < 2; i++ {
		_, done[i] = e.helper("upload", dir)
		st, m := acceptFile(t, b)
		sendTest(t, st, message{Type: "offer", ID: m.ID, Name: fmt.Sprintf("file%d", i), Size: 100})
		untilType(t, st, "accept")
		sendTest(t, st, message{Type: "chunk", ID: m.ID, Data: []byte("partial")})
	}
	b.close()
	for _, d := range done {
		waitHelper(t, d, false)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("partial uploads leaked", entries)
	}
	_, newPID := e.attach("main")
	if newPID != pid {
		t.Fatal("disconnect replaced shell")
	}
}

func TestBatchDownloadsContinueAfterFailure(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	args := []string{"download", filepath.Join(e.dir, "does-not-exist")}
	expected := make(map[string][]byte)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("batch-%d.bin", i)
		data := bytes.Repeat([]byte{byte(i)}, i*100)
		path := filepath.Join(e.dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
		expected[name] = data
	}
	c := exec.Command(os.Args[0], args...)
	c.Env = append(e.env, "SSHORE_TEST_WIRE_HELPER=1", "SSHORE_SOCKET="+filepath.Join(e.dir, "main.sock"))
	var logs bytes.Buffer
	c.Stderr = &logs
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for range expected {
		st, offer := acceptFile(t, b)
		sendTest(t, st, message{Type: "accept", ID: offer.ID})
		var got []byte
		for {
			m := recv(t, st)
			if m.Type == "end" {
				break
			}
			if m.Type != "chunk" {
				t.Fatal(m.Type)
			}
			got = append(got, m.Data...)
		}
		if !bytes.Equal(got, expected[offer.Name]) {
			t.Fatal("wrong batch content", offer.Name)
		}
		sendTest(t, st, message{Type: "result", ID: offer.ID})
		_ = st.Close()
	}
	waitHelper(t, done, false)
	if !strings.Contains(logs.String(), "1 of 9 downloads failed") || strings.Contains(logs.String(), "may be active") {
		t.Fatal(logs.String())
	}
}
func TestInterruptedUploadHelperRemovesPartialFile(t *testing.T) {
	e := newEnv(t)
	b, _ := e.attach("main")
	dir := filepath.Join(e.dir, "uploads")
	_ = os.Mkdir(dir, 0700)
	c, done := e.helper("upload", dir)
	st, m := acceptFile(t, b)
	sendTest(t, st, message{Type: "offer", ID: m.ID, Name: "interrupted.bin", Size: 100})
	untilType(t, st, "accept")
	sendTest(t, st, message{Type: "chunk", ID: m.ID, Data: []byte("partial")})
	if err := c.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitHelper(t, done, false)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("interrupted helper left a partial file", entries, err)
	}
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
