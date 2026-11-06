//go:build linux

package main

// Each named session has one daemon and PTY; there is no global daemon or extra
// listening network port. The bridge connects SSH stdin/stdout to a Unix socket,
// starting the daemon as needed. Only one client can attach. Names are 1-48 ASCII
// letters, digits, underscores or hyphens and begin with a letter or digit.
// Omitted names allocate a fresh mascot session; explicit names create or resume.
// The daemon drains/discards detached or muted output, so
// an unread terminal cannot block remote programs. No screen or scrollback is
// stored. Shell exit ends the session; system cleanup/reboot can also end it.
//
// Runtime selection: SSHORE_RUNTIME_DIR overrides XDG_RUNTIME_DIR/sshore, with
// /tmp/sshore-UID as fallback. The directory must be current-user-owned and 0700;
// custom paths must be absolute with an existing parent and short enough for Unix
// socket limits. .sock is the endpoint; .lock/.start coordinate lifetime/startup;
// .log records background errors. Locks/logs may outlive the session. Listing
// queries sockets rather than treating leftover files as live sessions. The shell
// receives SSHORE_SOCKET for remote helpers. Test lifecycle binaries must use
// -tags=sshoretest for runtime isolation; release binaries must not use that tag.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/creack/pty"
	"github.com/xtaci/smux"
	"golang.org/x/sys/unix"
)

func runtimeDir() (string, error) {
	dir := os.Getenv("SSHORE_RUNTIME_DIR")
	if dir == "" {
		if root := os.Getenv("XDG_RUNTIME_DIR"); root != "" {
			dir = filepath.Join(root, "sshore")
		} else {
			dir = fmt.Sprintf("/tmp/sshore-%d", os.Geteuid())
		}
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("runtime directory must be absolute")
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("runtime directory must be owned by you with mode 0700: %s", dir)
	}
	return dir, nil
}
func socketPath(name string) (string, error) {
	if err := validSession(name); err != nil {
		return "", err
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".sock")
	if len(path) >= 104 {
		return "", fmt.Errorf("runtime socket path is too long")
	}
	return path, nil
}
func openLock(path string, nonblocking bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	flags := unix.LOCK_EX
	if nonblocking {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(fd, flags); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Automatic and explicit selection share a startup lock until the daemon listens.
// Live sessions, including detached ones, keep their names. The account setup
// gate also serializes allocation against setup and other bridge starts.
func reserveSession(names []string, fresh bool) (string, *os.File, net.Conn, error) {
	for _, name := range names {
		path, err := socketPath(name)
		if err != nil {
			return "", nil, nil, err
		}
		startup, err := openLock(path+".start", false)
		if err != nil {
			return "", nil, nil, err
		}
		conn, err := net.Dial("unix", path)
		if fresh && err == nil {
			conn.Close()
			startup.Close()
			continue
		}
		return name, startup, conn, nil
	}
	return "", nil, nil, fmt.Errorf("all automatic session names are in use; exit an existing session or choose a name with --session")
}

func bridge(name string, existing bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lock, err := setupLock()
	if err != nil {
		return err
	}
	state, err := inspectSetup(exe)
	if err != nil {
		lock.Close()
		return err
	}
	if state.Manual || len(state.Processes) > 0 || state.Installed != version {
		lock.Close()
		return fmt.Errorf("remote versions differ; reconnect through setup")
	}
	return bridgePrepared(name, existing, exe, lock)
}

func bridgePrepared(name string, existing bool, executable string, gate *os.File) error {
	defer gate.Close()
	if existing && name == "" {
		return fmt.Errorf("approved session requires an explicit name")
	}
	names := []string{name}
	fresh := name == ""
	if fresh {
		names = automaticSessionNames()
	}
	name, startup, conn, err := reserveSession(names, fresh)
	if err != nil {
		return err
	}
	defer startup.Close()
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	if conn == nil {
		if existing {
			return fmt.Errorf("approved session no longer exists")
		}
		exe := executable
		fd, err := unix.Open(path+".log", unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		log := os.NewFile(uintptr(fd), path+".log")
		cmd := exec.Command(exe, "daemon", name)
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = cmd.Start()
		log.Close()
		if err != nil {
			return err
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		deadline := time.After(3 * time.Second)
		retry := time.NewTicker(20 * time.Millisecond)
		defer retry.Stop()
	wait:
		for {
			select {
			case err := <-exited:
				return fmt.Errorf("session daemon exited: %v; see %s.log", err, path)
			case <-deadline:
				return fmt.Errorf("starting session timed out; see %s.log", path)
			case <-retry.C:
				conn, err = net.Dial("unix", path)
				if err == nil {
					break wait
				}
			}
		}
	}
	_ = unix.Flock(int(startup.Fd()), unix.LOCK_UN)
	defer conn.Close()
	gate.Close()
	reportSessionDaemons()
	go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
	_, err = io.Copy(os.Stdout, conn)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Only the terminal attachment owns a mux session. Local helper commands keep
// using one ordinary Unix socket each; their bytes are relayed into a dedicated
// smux stream, without passing through the PTY or the control stream.
type attachment struct {
	mux               *smux.Session
	control, terminal *smux.Stream
	controlMu         sync.Mutex
	terminalWriteMu   sync.Mutex
	forwardRequests   chan struct{}
	muted             atomic.Bool
}

func (p *attachment) controlMessage(m message) error {
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	return streamMessage(p.control, m)
}
func (p *attachment) output(m message) error {
	p.terminalWriteMu.Lock()
	defer p.terminalWriteMu.Unlock()
	if m.Type == "output" && p.muted.Load() {
		return nil
	}
	return streamMessage(p.terminal, m)
}
func (p *attachment) close() { _ = p.mux.Close() }

type activeTransfer struct {
	id     uint64
	helper net.Conn
	stream *smux.Stream
	owner  *attachment
}

func (t *activeTransfer) close() {
	_ = t.helper.Close()
	if t.stream != nil {
		_ = t.stream.Close()
	}
}

type session struct {
	plain      bool
	terminalMu sync.Mutex // serializes state snapshots with PTY output
	mouse      terminalMouseState
	mu         sync.Mutex
	active     *attachment
	transfers  map[uint64]*activeTransfer
	master     *os.File
	cmd        *exec.Cmd
	exited     bool
	path       string
	done       chan struct{}
	once       sync.Once
}

func (s *session) stop() { s.once.Do(func() { close(s.done) }) }
func (s *session) start(cols, rows uint16, term string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-i")
	cmd.Env = append(os.Environ(), "TERM="+term, "SSHORE_SOCKET="+s.path)
	if s.plain {
		cmd.Env = nil
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "SSHORE_") {
				cmd.Env = append(cmd.Env, item)
			}
		}
		cmd.Env = append(cmd.Env, "TERM="+term, "SSHORE_DISABLED=1")
	}
	if exe, err := os.Executable(); err == nil && !s.plain {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(exe)+":"+os.Getenv("PATH"))
	}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: max(cols, 1), Rows: max(rows, 1)})
	if err != nil {
		return err
	}
	s.cmd, s.master = cmd, master
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.terminalMu.Lock()
				s.mouse.observe(buf[:n])
				s.mu.Lock()
				p := s.active
				s.mu.Unlock()
				if p != nil {
					if p.output(message{Type: "output", Data: buf[:n]}) != nil {
						p.close()
					}
				}
				s.terminalMu.Unlock()
			}
			if err != nil {
				break
			}
		}
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = 1
			var e *exec.ExitError
			if errors.As(err, &e) {
				code = e.ExitCode()
			}
		}
		s.mu.Lock()
		p := s.active
		s.active = nil
		s.exited = true
		s.mu.Unlock()
		if p != nil {
			// Exit shares the terminal stream, preserving all final output ordering.
			_ = p.output(message{Type: "exit", Code: code})
			select {
			case <-p.mux.CloseChan():
			case <-time.After(2 * time.Second):
				p.close()
			}
		}
		s.stop()
	}()
	return nil
}
func (s *session) attach(p *attachment, m message) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if m.Type != "attach" {
		return fmt.Errorf("expected attach request")
	}
	if len(m.Term) == 0 || len(m.Term) > 128 || strings.ContainsAny(m.Term, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid TERM")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return fmt.Errorf("session is exiting; reconnect to create a new one")
	}
	if s.active != nil {
		return fmt.Errorf("session already attached; detach the other connection first")
	}
	if m.Action == "approved" {
		if s.cmd == nil || s.cmd.Process.Pid != m.PID {
			return fmt.Errorf("approved session changed")
		}
		p.muted.Store(true)
	}
	reconnecting := s.master != nil
	if s.master == nil {
		if err := s.start(m.Cols, m.Rows, m.Term); err != nil {
			return err
		}
	}
	if err := pty.Setsize(s.master, &pty.Winsize{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
		return err
	}
	if err := p.controlMessage(message{Type: "ready", PID: s.cmd.Process.Pid, Name: strings.TrimSuffix(filepath.Base(s.path), ".sock")}); err != nil {
		return err
	}
	if !p.muted.Load() {
		if data := s.mouse.replay(); len(data) > 0 {
			if err := p.output(message{Type: "output", Data: data}); err != nil {
				return err
			}
		}
	}
	s.active = p
	if reconnecting && !p.muted.Load() {
		requestTerminalRedraw(s.master)
	}
	return nil
}
func (s *session) release(p *attachment) {
	p.close()
	s.mu.Lock()
	if s.active == p {
		s.active = nil
	}
	var tasks []*activeTransfer
	for id, t := range s.transfers {
		if t.owner == p {
			tasks = append(tasks, t)
			delete(s.transfers, id)
		}
	}
	s.mu.Unlock()
	for _, t := range tasks {
		t.close()
	}
}
func (s *session) detach(p *attachment) {
	s.mu.Lock()
	if s.active == p {
		s.active = nil
	}
	var tasks []*activeTransfer
	for id, t := range s.transfers {
		if t.owner == p {
			tasks = append(tasks, t)
			delete(s.transfers, id)
		}
	}
	s.mu.Unlock()
	for _, t := range tasks {
		t.close()
	}
	_ = p.controlMessage(message{Type: "detached"})
	// Let the client acknowledge the final frame before closing the mux transport.
	// Closing a mux session immediately can discard unread stream buffers.
	_ = p.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = readMessage(p.control)
	p.close()
}
func (s *session) serveMux(c io.ReadWriteCloser) error {
	mux, err := smux.Server(c, muxConfig())
	if err != nil {
		return err
	}
	defer mux.Close()
	_ = mux.SetDeadline(time.Now().Add(5 * time.Second))
	control, err := mux.AcceptStream()
	if err != nil {
		return err
	}
	_ = mux.SetDeadline(time.Time{})
	_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := readMessage(control)
	if err != nil {
		return err
	}
	_ = control.SetReadDeadline(time.Time{})
	terminal, err := mux.OpenStream()
	if err != nil {
		return err
	}
	if err = streamMessage(terminal, message{Type: "terminal"}); err != nil {
		return err
	}
	p := &attachment{mux: mux, control: control, terminal: terminal, forwardRequests: make(chan struct{}, 8)}
	defer s.release(p)
	if err = s.attach(p, m); err != nil {
		_ = p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(control)
		return err
	}
	forwardCtx, cancelForwards := context.WithCancel(context.Background())
	defer cancelForwards()
	if !s.plain {
		go serveForwardStreams(forwardCtx, mux)
	} else {
		go func() {
			for {
				stream, e := mux.AcceptStream()
				if e != nil {
					return
				}
				_ = stream.Close()
			}
		}()
	}
	go func() {
		for {
			m, err := readMessage(terminal)
			if err != nil {
				p.close()
				return
			}
			if m.Type != "input" {
				p.close()
				return
			}
			s.mu.Lock()
			allowed := s.active == p
			master := s.master
			s.mu.Unlock()
			if !allowed {
				return
			}
			if err = writeFull(master, m.Data); err != nil {
				p.close()
				return
			}
		}
	}()
	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "resume_output":
			s.terminalMu.Lock()
			if p.muted.Swap(false) {
				if data := s.mouse.replay(); len(data) > 0 {
					err = p.output(message{Type: "output", Data: data})
				}
				s.mu.Lock()
				if err == nil && s.active == p {
					requestTerminalRedraw(s.master)
				}
				s.mu.Unlock()
			}
			s.terminalMu.Unlock()
			if err != nil {
				return err
			}
			if err := p.controlMessage(message{Type: "output_resumed"}); err != nil {
				return err
			}
		case "resize":
			s.mu.Lock()
			if s.active == p {
				_ = pty.Setsize(s.master, &pty.Winsize{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
			}
			s.mu.Unlock()
			if err := p.controlMessage(message{Type: "resized", Cols: m.Cols, Rows: m.Rows}); err != nil {
				return err
			}
		case "detach":
			s.detach(p)
			return nil
		case "cancel":
			s.mu.Lock()
			t := s.transfers[m.ID]
			if t != nil && t.owner == p {
				delete(s.transfers, m.ID)
			} else {
				t = nil
			}
			s.mu.Unlock()
			if t != nil {
				t.close()
			}
		default:
			return fmt.Errorf("unexpected control message: %s", m.Type)
		}
	}
}
func (s *session) transfer(id uint64, c net.Conn, m message) error {
	s.mu.Lock()
	if s.active == nil {
		s.mu.Unlock()
		return fmt.Errorf("no attached client")
	}
	if len(s.transfers) >= maxTransfers {
		s.mu.Unlock()
		return fmt.Errorf("at most %d file transfers may be active", maxTransfers)
	}
	p := s.active
	t := &activeTransfer{id: id, helper: c, owner: p}
	s.transfers[id] = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.transfers[id] == t {
			delete(s.transfers, id)
		}
		s.mu.Unlock()
		t.close()
	}()
	st, err := p.mux.OpenStream()
	if err != nil {
		return err
	}
	s.mu.Lock()
	// Detach/cancellation may have removed the reservation while OpenStream ran.
	if s.transfers[id] != t {
		s.mu.Unlock()
		_ = st.Close()
		return fmt.Errorf("transfer cancelled")
	}
	t.stream = st
	s.mu.Unlock()
	m.ID = id
	if err = streamMessage(st, m); err != nil {
		return err
	}
	// Release the slot before forwarding a terminal result, so a batch can
	// start its next file as soon as the previous helper reports success.
	releaseSlot := func() {
		s.mu.Lock()
		if s.transfers[id] == t {
			delete(s.transfers, id)
		}
		s.mu.Unlock()
	}
	relay := func(dst io.Writer, src io.Reader) error {
		for {
			m, e := readMessage(src)
			if e != nil {
				return e
			}
			if m.ID != id {
				return fmt.Errorf("wrong transfer ID")
			}
			final := m.Type == "result" || m.Type == "error" || m.Type == "cancel"
			if final {
				releaseSlot()
			}
			if e = writeMessage(dst, m); e != nil {
				return e
			}
			if final {
				return nil
			}
		}
	}
	done := make(chan error, 2)
	go func() { done <- relay(st, c) }()
	go func() { done <- relay(c, st) }()
	err = <-done
	t.close() // Closing one file stream unblocks both directions, not its siblings.
	<-done
	return err
}
func (s *session) handle(id uint64, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	first, err := readMessage(c)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	switch first.Type {
	case "mux":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
			return
		}
		if writeMessage(c, message{Type: "mux_ready", SoftwareVersion: version}) == nil {
			_ = s.serveMux(c)
		}
	case "status":
		s.mu.Lock()
		m := message{Type: "session", Attached: s.active != nil}
		group := 0
		if s.master != nil {
			group, _ = unix.IoctlGetInt(int(s.master.Fd()), unix.TIOCGPGRP)
		}
		if s.cmd != nil {
			m.PID = s.cmd.Process.Pid
		}
		s.mu.Unlock()
		if first.Action != "health" {
			m.Foreground = foregroundPrograms("/proc", group)
		}
		_ = writeMessage(c, m)
	case "detach":
		s.mu.Lock()
		p := s.active
		s.mu.Unlock()
		// Ask the attachment's control loop to handle finalization, avoiding two
		// concurrent readers of the control stream.
		if p != nil {
			_ = p.controlMessage(message{Type: "detach_requested"})
		}
		_ = writeMessage(c, message{Type: "detached"})
	case "approval_request":
		if err = s.requestApproval(c, first); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
		}
	case "forward":
		if err = s.forward(c, first); err != nil {
			_ = writeMessage(c, message{Type: "forward_result", Error: err.Error()})
		}
	case "upload", "offer":
		if err = s.transfer(id, c, first); err != nil {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
	default:
		_ = writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
}
func daemon(name string) error {
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	lock, err := openLock(path+".lock", true)
	if err != nil {
		return fmt.Errorf("session already running: %w", err)
	}
	defer lock.Close()
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	s := &session{path: path, done: make(chan struct{}), transfers: make(map[uint64]*activeTransfer)}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.master != nil {
			_ = s.master.Close()
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Catch and discard HUP in the daemon, rather than installing SIG_IGN:
	// ignored dispositions survive exec and could make a newly started shell
	// ignore HUP before its own signal handlers are installed.
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.done:
		}
		_ = listener.Close()
	}()
	// A failed initial attach should not leave an empty daemon indefinitely.
	go func() {
		select {
		case <-time.After(30 * time.Second):
			s.mu.Lock()
			empty := s.master == nil
			s.mu.Unlock()
			if empty {
				s.stop()
			}
		case <-s.done:
		}
	}()
	var id atomic.Uint64
	for {
		c, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return err
			}
		}
		go s.handle(id.Add(1), c)
	}
}
func listSessions() error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer out.Flush()
	fmt.Fprintln(out, "SESSION\tPID\tSTATE\tFOREGROUND")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		c, err := net.DialTimeout("unix", filepath.Join(dir, e.Name()), time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		err = writeMessage(c, message{Type: "status"})
		if err != nil {
			c.Close()
			continue
		}
		m, err := readMessage(c)
		c.Close()
		if err != nil || m.Type != "session" {
			continue
		}
		state := "detached"
		if m.Attached {
			state = "attached"
		}
		if m.Foreground == "" {
			m.Foreground = "—"
		}
		fmt.Fprintf(out, "%s\t%d\t%s\t%s\n", strings.TrimSuffix(e.Name(), ".sock"), m.PID, state, cleanText(m.Foreground))
	}
	return nil
}
func serverCommand(args []string) error {
	switch args[0] {
	case "bootstrap":
		if len(args) != 4 {
			return usage("invalid bootstrap arguments")
		}
		return bootstrap(args[1], args[2], args[3])
	case "bridge", "resume", "daemon":
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a session name")
		}
		if args[0] == "bridge" || args[0] == "resume" {
			return bridge(args[1], args[0] == "resume")
		}
		return daemon(args[1])
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
	case "forward":
		return forwardHelper(args[1:])
	case "detach":
		if len(args) != 1 {
			return usage("usage: sshore detach")
		}
		return helper("detach", "")
	case "upload":
		if len(args) > 2 {
			return usage("usage: sshore upload [DIRECTORY]")
		}
		path := "."
		if len(args) == 2 {
			path = args[1]
		}
		return requestFiles("upload", []string{path})
	case "download":
		if len(args) < 2 {
			return usage("usage: sshore download FILE...")
		}
		return requestFiles("download", args[1:])
	}
	return fmt.Errorf("unknown server command")
}
