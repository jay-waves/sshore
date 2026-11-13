//go:build linux

package main

// Each named session has one daemon and PTY; there is no global daemon or extra
// listening network port. The bridge connects SSH stdin/stdout to a Unix socket,
// starting the daemon as needed. Only one client can attach. Names are 1-48 ASCII
// letters, digits, underscores or hyphens and begin with a letter or digit.
// Public new always reserves a fresh name; attach requires an existing daemon.
// Omitted new names allocate automatically; omitted attach names are selected
// during setup. Legacy internal bridge calls retain their create-or-resume behavior.
// The daemon drains/discards detached or muted output, so
// an unread terminal cannot block remote programs. No screen or scrollback is
// stored. Only explicit detach preserves a session. Transport loss closes the PTY
// (normal terminal hangup semantics) and stops the daemon; shell exit also ends it.
// Programs deliberately ignoring/escaping hangup may survive, as with SSH.
//
// Runtime selection: SSHORE_RUNTIME_DIR overrides XDG_RUNTIME_DIR/sshore, with
// /tmp/sshore-UID as fallback. The directory must be current-user-owned and 0700;
// custom paths must be absolute with an existing parent and short enough for Unix
// socket limits. .sock is the endpoint; .lock/.start coordinate lifetime/startup;
// .log records background errors. Locks/logs may outlive the session. Listing
// queries sockets rather than treating leftover files as live sessions. Public
// labels may change while socket identities remain fixed; see session_metadata_linux.go.
// Bounded exit receipts are managed by session_exit_linux.go. The shell
// receives SSHORE_SOCKET for remote helpers. Test lifecycle binaries must use
// -tags=sshoretest for runtime isolation; release binaries must not use that tag.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
type sessionReservation struct {
	name, publicName string
	startup          *os.File
	conn             net.Conn
}

func reserveSession(names []string, fresh bool) (sessionReservation, error) {
	for _, name := range names {
		publicLabel := name
		path, err := resolveSession(name)
		if err != nil {
			return sessionReservation{}, err
		}
		if path != "" {
			if fresh {
				continue
			}
			name = strings.TrimSuffix(filepath.Base(path), ".sock")
		} else {
			path, err = socketPath(name)
			if err != nil {
				return sessionReservation{}, err
			}
			// A renamed daemon may still own this internal identity.
			if live, err := sessionIdentityLive(path); err != nil {
				return sessionReservation{}, err
			} else if live {
				name = "session-" + rand.Text()
				path, err = socketPath(name)
				if err != nil {
					return sessionReservation{}, err
				}
			}
		}
		startup, err := openLock(path+".start", false)
		if err != nil {
			return sessionReservation{}, err
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			live, liveErr := sessionIdentityLive(path)
			if live || liveErr != nil {
				startup.Close()
				return sessionReservation{}, fmt.Errorf("session %s unavailable: %w", publicLabel, errors.Join(err, liveErr))
			}
		}
		if fresh && err == nil {
			conn.Close()
			startup.Close()
			continue
		}
		return sessionReservation{name: name, publicName: publicLabel, startup: startup, conn: conn}, nil
	}
	return sessionReservation{}, fmt.Errorf("all automatic session names are in use; exit an existing session or choose a name with sshore new HOST NAME")
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
	return bridgePrepared(name, existing, false, exe, lock, "")
}

func bridgePrepared(name string, existing, freshOnly bool, executable string, gate *os.File, readyMode string) error {
	defer gate.Close()
	if existing && name == "" {
		return fmt.Errorf("attach requires a session name")
	}
	publicName := name
	if existing {
		found, err := resolveSession(name)
		if err != nil {
			return err
		}
		if found == "" {
			return fmt.Errorf("session %s does not exist; use sshore list HOST", name)
		}
	}
	names := []string{name}
	fresh := name == ""
	if fresh {
		names = automaticSessionNames()
	}
	reservation, err := reserveSession(names, fresh)
	if err != nil {
		return err
	}
	name, publicName = reservation.name, reservation.publicName
	startup, conn := reservation.startup, reservation.conn
	defer startup.Close()
	if freshOnly && conn != nil {
		conn.Close()
		return fmt.Errorf("session %s already exists; use sshore attach HOST %s", publicName, publicName)
	}
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	if conn == nil {
		if existing {
			return fmt.Errorf("session %s does not exist; use sshore list HOST or sshore new HOST NAME", name)
		}
		exe := executable
		fd, err := unix.Open(path+".log", unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		log := os.NewFile(uintptr(fd), path+".log")
		cmd := exec.Command(exe, "daemon", name)
		if publicName == "" {
			publicName = name
		}
		cmd.Env = append(os.Environ(), "SSHORE_SESSION_NAME="+publicName)
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
	if readyMode != "" {
		if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: readyMode}); err != nil {
			return err
		}
	}
	go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
	_, err = io.Copy(os.Stdout, conn)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Each attachment owns one mux session. Local helpers use Unix sockets for
// approval, forwarding control and detach requests. Approved file bytes flow
// directly on client-initiated streams, without a helper process or PTY relay.
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

type session struct {
	name       string
	exitReason string
	plain      bool
	terminalMu sync.Mutex // serializes state snapshots with PTY output
	mouse      terminalMouseState
	mu         sync.Mutex
	active     *attachment
	started    time.Time // shell lifetime, including time spent detached
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
	s.started = time.Now()
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
		if s.exitReason == "" {
			s.exitReason = fmt.Sprintf("shell exited (code %d)", code)
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				s.exitReason = "shell terminated by " + status.Signal().String()
			}
		}
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
		return fmt.Errorf("session is exiting; use sshore new HOST to create a new one")
	}
	if s.active != nil {
		return fmt.Errorf("session already attached; detach the other connection first")
	}
	if m.Action == "existing" && s.cmd == nil {
		return fmt.Errorf("session has not started; use sshore list HOST")
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
	if err := p.controlMessage(message{Type: "ready", PID: s.cmd.Process.Pid, Name: s.name}); err != nil {
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
	defer s.mu.Unlock()
	// Explicit detach already cleared active. Failed competing attachments must
	// never end the owner session. Mark it exiting before allowing any reattach.
	if s.active == p {
		s.active = nil
		s.exited = true
		if s.exitReason == "" {
			s.exitReason = "connection lost without detach"
		}
		if s.master != nil {
			_ = s.master.Close()
		}
		s.stop()
	}
}

func (s *session) clearAttachment(p *attachment) {
	s.mu.Lock()
	if s.active == p {
		s.active = nil
	}
	s.mu.Unlock()
}
func (s *session) detach(p *attachment) {
	s.clearAttachment(p)
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	_ = p.controlMessage(message{Type: "detached", Name: name})
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
		workersDone := make(chan struct{})
		go func() {
			defer close(workersDone)
			serveForwardStreams(forwardCtx, mux)
		}()
		defer func() {
			cancelForwards()
			p.close()
			// Let cancelled uploads remove temporary files before daemon exit.
			select {
			case <-workersDone:
			case <-time.After(2 * time.Second):
			}
		}()
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
		default:
			return fmt.Errorf("unexpected control message: %s", m.Type)
		}
	}
}
func (s *session) handle(c net.Conn) {
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
	case "rename":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		if err := s.rename(first.Action, first.Name); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		_ = writeMessage(c, message{Type: "renamed", Name: first.Name})
	case "status":
		s.mu.Lock()
		m := message{Type: "session", Attached: s.active != nil, Name: s.name, SoftwareVersion: version}
		group := 0
		if s.master != nil {
			group, _ = unix.IoctlGetInt(int(s.master.Fd()), unix.TIOCGPGRP)
		}
		if s.cmd != nil {
			m.PID = s.cmd.Process.Pid
			age := int64(time.Since(s.started) / time.Second)
			m.AgeSeconds = &age
		}
		s.mu.Unlock()
		if first.Action != "health" {
			m.Foreground = foregroundPrograms("/proc", group)
			m.Directory = sessionDirectory(group, m.PID)
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
	nameLabel := os.Getenv("SSHORE_SESSION_NAME")
	if validSession(nameLabel) != nil {
		nameLabel = name
	}
	if err := writeSessionName(path, nameLabel); err != nil {
		return err
	}
	defer os.Remove(path + ".name")
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
	s := &session{path: path, name: nameLabel, done: make(chan struct{})}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cmd != nil {
			reason := s.exitReason
			if reason == "" {
				reason = "daemon stopped (signal or shutdown)"
			}
			saveSessionExit(s.path, s.name, reason)
		}
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
		go s.handle(c)
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
	out := os.Stdout
	count := 0
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
		if count > 0 {
			fmt.Fprintln(out)
		}
		count++
		fmt.Fprintf(out, "%s [%s]%s\n", sessionLabel(m, e.Name()), state, sessionAge(m.AgeSeconds))
		fmt.Fprintf(out, "  %s · %s\n", cleanText(m.Foreground), cleanText(m.Directory))
		fmt.Fprint(out, sessionProcessTree("/proc", m.PID))
	}
	if count == 0 {
		fmt.Fprintln(out, "No sessions")
	}
	return printSessionExits(dir, out)
}
func serverCommand(args []string) error {
	switch args[0] {
	case "rename-session":
		if len(args) != 4 {
			return usage("rename-session requires old name, new name and client version")
		}
		if err := checkPeerVersion(args[3], "client"); err != nil {
			return err
		}
		return renameSession(args[1], args[2])
	case "kill-session":
		if len(args) != 3 {
			return usage("kill-session requires a name and client version")
		}
		if err := checkPeerVersion(args[2], "client"); err != nil {
			return err
		}
		return killSession(args[1])
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
		return detachHelper()
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

// Candidates are read-only status probes in this account's current runtime
// directory. Selection is advisory: the bridge and daemon recheck existence and
// exclusive attachment after the user answers, without holding the setup lock.
func detachedSessionNames() ([]string, error) {
	dir, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sock")
		if validSession(name) != nil {
			continue
		}
		c, err := net.DialTimeout("unix", filepath.Join(dir, e.Name()), time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		err = writeMessage(c, message{Type: "status"})
		var m message
		if err == nil {
			m, err = readMessage(c)
		}
		c.Close()
		if err == nil && m.Type == "session" && !m.Attached && m.PID > 0 {
			names = append(names, sessionLabel(m, e.Name()))
			if len(names) > 128 {
				return nil, fmt.Errorf("too many sessions to select; use sshore list HOST, then sshore attach HOST NAME")
			}
		}
	}
	return names, nil
}
