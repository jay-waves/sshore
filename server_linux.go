//go:build linux

package main

// A shared remote daemon owns each named session's PTY. The bridge connects
// SSH stdin/stdout to a per-session Unix socket, starting the daemon as needed.
// Only one client can attach to each shell. Names are 1-48 ASCII
// letters, digits, underscores or hyphens and begin with a letter or digit.
// Public new always reserves a fresh name; attach requires an existing daemon.
// Omitted new names allocate automatically; omitted attach names are selected
// during setup. Legacy internal bridge calls retain their create-or-resume behavior.
// The daemon drains/discards detached or muted output, so
// an unread terminal cannot block remote programs. No screen or scrollback is
// stored. Transport loss releases the attachment while preserving the shell.
// Shell exit or explicit cancellation ends the session.
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
	"encoding/json"
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
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/tmux"
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
	namespace        *resourceNameReservation
	conn             net.Conn
}

func (r *sessionReservation) Close() {
	if r.startup != nil {
		r.startup.Close()
	}
	if r.namespace != nil {
		r.namespace.Close()
	}
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
		var namespace *resourceNameReservation
		if found, err := resolveSession(publicLabel); err != nil {
			return sessionReservation{}, err
		} else if found == "" {
			namespace, err = reserveResourceName("shell", publicLabel)
			if err != nil {
				if fresh && errors.Is(err, errObjectNameInUse) {
					continue
				}
				return sessionReservation{}, err
			}
		}
		startup, err := openLock(path+".start", false)
		if err != nil {
			if namespace != nil {
				namespace.Close()
			}
			return sessionReservation{}, err
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			live, liveErr := sessionIdentityLive(path)
			if live || liveErr != nil {
				startup.Close()
				if namespace != nil {
					namespace.Close()
				}
				return sessionReservation{}, fmt.Errorf("shell %s unavailable: %w", publicLabel, errors.Join(err, liveErr))
			}
		}
		if fresh && err == nil {
			conn.Close()
			startup.Close()
			if namespace != nil {
				namespace.Close()
			}
			continue
		}
		return sessionReservation{name: name, publicName: publicLabel, startup: startup, namespace: namespace, conn: conn}, nil
	}
	return sessionReservation{}, fmt.Errorf("all automatic shell names are in use; exit an existing shell or choose a name with sshore launch HOST NAME")
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

func bridgePrepared(name string, existing, freshOnly bool, executable string, gate *os.File, readyMode string, temporary ...bool) error {
	defer gate.Close()
	if existing && name == "" {
		return fmt.Errorf("attach requires a shell name")
	}
	publicName := name
	if existing {
		found, err := resolveSession(name)
		if err != nil {
			return err
		}
		if found == "" {
			return fmt.Errorf("shell %s does not exist; use sshore list HOST", name)
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
	defer reservation.Close()
	if freshOnly && conn != nil {
		conn.Close()
		return fmt.Errorf("shell %s already exists; use sshore attach HOST %s", publicName, publicName)
	}
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	if conn == nil {
		if existing {
			return fmt.Errorf("shell %s does not exist; use sshore list HOST or sshore launch HOST NAME", name)
		}
		if reservation.namespace != nil {
			reservation.namespace.owner.Close()
			reservation.namespace.owner = nil
		}
		if publicName == "" {
			publicName = name
		}
		if err := createRemoteShell(executable, name, publicName, len(temporary) > 0 && temporary[0]); err != nil {
			return err
		}
		conn, err = net.DialTimeout("unix", path, time.Second)
		if err != nil {
			return err
		}

	}
	_ = unix.Flock(int(startup.Fd()), unix.LOCK_UN)
	if reservation.namespace != nil {
		reservation.namespace.Close()
		reservation.namespace = nil
	}
	defer conn.Close()
	gate.Close()
	if len(temporary) > 0 && temporary[0] {
		return bridgeSpawn(path, conn, readyMode)
	}
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
type shell struct {
	temporary       bool
	spawnOwned      bool
	environment     []string
	directory       string
	processIdentity string
	cancelling      bool

	instance   string
	name       string
	nameLock   *os.File
	exitReason string
	plain      bool
	terminalMu sync.Mutex // serializes state snapshots with PTY output
	mouse      tmux.InputState
	mu         sync.Mutex
	lifecycle  tmux.Lifecycle[*attachment]
	started    time.Time // shell lifetime, including time spent detached
	master     *tmux.Terminal
	cmd        *exec.Cmd
	path       string
	done       chan struct{}
	once       sync.Once
}

func (s *shell) stop() { s.once.Do(func() { close(s.done) }) }
func (s *shell) start(cols, rows uint16, term string) error {
	if s.instance == "" {
		s.instance = rand.Text()
	}
	env := s.environment
	if env == nil {
		env = os.Environ()
	}
	shell := localEnvironmentValue(env, "SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-i")
	cmd.Dir = s.directory
	cmd.Env = append(append([]string(nil), env...), "TERM="+term, "SSHORE_SOCKET="+s.path)
	if s.plain {
		cmd.Env = nil
		for _, item := range env {
			if !strings.HasPrefix(item, "SSHORE_") {
				cmd.Env = append(cmd.Env, item)
			}
		}
		cmd.Env = append(cmd.Env, "TERM="+term, "SSHORE_DISABLED=1")
	}
	if exe, err := os.Executable(); err == nil && !s.plain {
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(exe)+":"+localEnvironmentValue(env, "PATH"))
	}
	master, err := tmux.StartCommand(cmd, tmux.Size{Cols: max(cols, 1), Rows: max(rows, 1)})
	if err != nil {
		return err
	}
	s.cmd, s.master = cmd, master
	s.processIdentity, err = processStart(cmd.Process.Pid)
	if err != nil {
		master.Close()
		return err
	}
	s.started = time.Now()
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.terminalMu.Lock()
				s.mouse.Observe(buf[:n])
				s.mu.Lock()
				p := s.lifecycle.Active()
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
		err := master.Wait()
		code := 0
		if err != nil {
			code = 1
			var e *exec.ExitError
			if errors.As(err, &e) {
				code = e.ExitCode()
			}
		}
		s.mu.Lock()
		p := s.lifecycle.End()
		if s.exitReason == "" {
			s.exitReason = fmt.Sprintf("shell exited (code %d)", code)
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				s.exitReason = "shell terminated by " + status.Signal().String()
			}
		}
		s.mu.Unlock()
		if p != nil {
			p.finishExit(code)
		}
		s.stop()
	}()
	return nil
}
func (s *shell) attach(p *attachment, m message) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if err := validateAttach(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	startedHere := false
	err := s.lifecycle.Attach(p, func() error {
		if s.cancelling {
			return fmt.Errorf("shell is being cancelled")
		}
		if m.Instance != "" && m.Instance != s.instance {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
		if m.Action == "existing" && s.cmd == nil {
			return fmt.Errorf("shell has not started; use sshore list HOST")
		}
		if m.Action == "approved" {
			if s.cmd == nil || s.cmd.Process.Pid != m.PID {
				return fmt.Errorf("approved shell changed")
			}
			p.muted.Store(true)
		}
		reconnecting := s.master != nil
		if s.master == nil {
			if err := s.start(m.Cols, m.Rows, m.Term); err != nil {
				return err
			}
			startedHere = true
		}
		if err := s.master.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
			return err
		}
		if err := p.controlMessage(message{Type: "ready", Temporary: s.temporary, Instance: s.instance, PID: s.cmd.Process.Pid, Name: s.name}); err != nil {
			return err
		}
		if !p.muted.Load() {
			if data := s.mouse.Replay(); len(data) > 0 {
				if err := p.output(message{Type: "output", Data: data}); err != nil {
					return err
				}
			}
		}
		p.input = tmux.NewInputLease(s.master)
		if reconnecting && !p.muted.Load() {
			tmux.RequestRedraw(s.master, s.cmd.Process.Pid)
		}
		return nil
	})
	if err != nil && startedHere {
		s.lifecycle.End()
		s.exitReason = "initial attachment failed"
		_ = s.master.Close()
		s.stop()
	}
	if errors.Is(err, tmux.ErrExited) {
		return fmt.Errorf("shell is exiting; use sshore launch HOST to create a new one")
	}
	if errors.Is(err, tmux.ErrAttached) {
		return fmt.Errorf("shell already attached; detach the other connection first")
	}
	return err
}
func (s *shell) release(p *attachment) {
	p.close()
	// Stop this attachment's writer before allowing another client to attach.
	s.clearAttachment(p)
}

func (s *shell) clearAttachment(p *attachment) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Active() == p && p.input != nil {
		if err := p.input.Stop(); err != nil {
			s.lifecycle.Release(p)
			s.exitReason = "terminal input cancellation failed"
			s.stop()
			return false
		}
	}
	return s.lifecycle.Detach(p)
}
func (s *shell) detach(p *attachment) {
	if !s.clearAttachment(p) {
		p.close()
		return
	}
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	tmux.FinishDetach(p.control, func() error { return p.controlMessage(message{Type: "detached", Name: name}) }, p.close)
}
func (s *shell) serveMux(c io.ReadWriteCloser) error {
	mux, control, terminal, m, err := acceptMux(c)
	if err != nil {
		return err
	}
	defer mux.Close()
	p := &attachment{mux: mux, control: control, terminal: terminal, approvalRequests: make(chan struct{}, 8)}
	defer s.release(p)
	if err = s.attach(p, m); err != nil {
		_ = p.controlMessage(message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(control)
		return err
	}
	if m.Action == "approved" && validApprovalID(m.ApprovalID) {
		_ = removeRemoteTask(m.ApprovalID)
	}
	operationCtx, cancelOperations := context.WithCancel(context.Background())
	defer cancelOperations()
	if !s.plain {
		workersDone := make(chan struct{})
		go func() {
			defer close(workersDone)
			serveApprovedStreams(operationCtx, mux)
		}()
		defer func() {
			cancelOperations()
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
	go p.readInput(&s.lifecycle)

	for {
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		switch m.Type {
		case "resume_output":
			s.terminalMu.Lock()
			if p.muted.Swap(false) {
				if data := s.mouse.Replay(); len(data) > 0 {
					err = p.output(message{Type: "output", Data: data})
				}
				s.mu.Lock()
				if err == nil && s.lifecycle.Active() == p {
					tmux.RequestRedraw(s.master, s.cmd.Process.Pid)
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
		case "inventory":
			inv, err := localSessionInventory()
			if err != nil {
				inv.Errors = append(inv.Errors, cleanText(err.Error()))
			}
			for _, row := range inv.Shells {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Shells: []shellView{row}}}); err != nil {
					return err
				}
			}
			for _, row := range inv.Resources {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Resources: []resourceView{row}}}); err != nil {
					return err
				}
			}
			for _, row := range inv.Peers {
				if err := p.controlMessage(message{Type: "inventory_row", Inventory: &inventory{Peers: []peerView{row}}}); err != nil {
					return err
				}
			}
			if err := p.controlMessage(message{Type: "inventory_done", Inventory: &inventory{Errors: inv.Errors}}); err != nil {
				return err
			}
		case "session_targets":
			if err := p.controlMessage(s.sessionTargets(m)); err != nil {
				return err
			}
		case "redraw":
			s.terminalMu.Lock()
			s.mu.Lock()
			if s.lifecycle.Active() == p && !p.muted.Load() {
				tmux.RequestRedraw(s.master, s.cmd.Process.Pid)
			}
			s.mu.Unlock()
			s.terminalMu.Unlock()
			if err := p.controlMessage(message{Type: "redrawn"}); err != nil {
				return err
			}
		case "resize":
			s.mu.Lock()
			var resizeErr error
			if s.lifecycle.Active() == p {
				resizeErr = s.master.Resize(tmux.Size{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)})
			}
			s.mu.Unlock()
			if resizeErr != nil {
				return fmt.Errorf("resize terminal: %w", resizeErr)
			}
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
func (s *shell) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	first, err := readMessage(c)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	switch first.Type {
	case "spawn_owner":
		s.serveSpawnOwner(c, first)
	case "mux":
		s.mu.Lock()
		unowned := s.temporary && !s.spawnOwned
		s.mu.Unlock()
		if unowned {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "spawn lifetime is not established"})
			return
		}
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
			return
		}
		if writeMessage(c, message{Type: "mux_ready", SoftwareVersion: version}) == nil {
			_ = s.serveMux(c)
		}
	case "cancel":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		if err := s.cancel(first.Instance); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		_ = writeMessage(c, message{Type: "cancelled"})
	case "forward_check":
		reply := message{Type: "forward_checked", SoftwareVersion: version}
		if err := s.checkForwardApproval(first); err != nil {
			reply.Type = "error"
			reply.Error = err.Error()
		}
		_ = writeMessage(c, reply)
	case "rename":
		if err := checkPeerVersion(first.SoftwareVersion, "client"); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		if err := s.rename(first.Action, first.Name, first.Instance); err != nil {
			_ = writeMessage(c, message{Type: "error", Error: err.Error()})
			return
		}
		_ = writeMessage(c, message{Type: "renamed", Name: first.Name})
	case "status":
		s.mu.Lock()
		m := message{Type: "session", Temporary: s.temporary, Instance: s.instance, Attached: s.lifecycle.Active() != nil, Name: s.name, SoftwareVersion: version}
		group := 0
		if s.master != nil {
			group, _ = s.master.ForegroundGroup()
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
		p := s.lifecycle.Active()
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
	default:
		_ = writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
}
func daemon(name string) error {
	label := os.Getenv("SSHORE_SESSION_NAME")
	if validSession(label) != nil {
		label = name
	}
	return serveRemoteShell(name, label, os.Environ(), "", nil)
}

func serveRemoteShell(name, nameLabel string, env []string, directory string, ready func() error, temporary ...bool) error {
	path, err := socketPath(name)
	if err != nil {
		return err
	}
	lock, err := openLock(path+".lock", true)
	if err != nil {
		return fmt.Errorf("shell already running: %w", err)
	}
	defer lock.Close()
	nameLock, err := lockObjectName(nameLabel)
	if err != nil {
		return fmt.Errorf("shell name %s is in use: %w", nameLabel, err)
	}
	if err := writeSessionName(path, nameLabel); err != nil {
		nameLock.Close()
		return err
	}
	defer func() {
		if nameLock != nil {
			nameLock.Close()
		}
	}()
	defer os.Remove(path + ".name")
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	var handlers sync.WaitGroup
	defer handlers.Wait()
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	s := &shell{temporary: len(temporary) > 0 && temporary[0], instance: rand.Text(), environment: env, directory: directory, path: path, name: nameLabel, nameLock: nameLock, done: make(chan struct{})}
	nameLock = nil
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.nameLock != nil {
			s.nameLock.Close()
		}
	}()
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
	if ready != nil {
		if err := ready(); err != nil {
			s.stop()
			return err
		}
	}
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
		handlers.Add(1)
		go func() { defer handlers.Done(); s.handle(c) }()
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
		fmt.Fprintln(out, "No shells")
	}
	if err = printSessionExits(dir, out); err != nil {
		return err
	}
	return listRemoteTasks()
}
func serverCommand(args []string) error {
	switch args[0] {
	case "forward-watch":
		return runApprovedForwardWatch(args[1:])
	case "mount-agent":
		if len(args) != 1 {
			return usage("mount-agent takes no arguments")
		}
		return files.ServeAgent(&pipeTransport{Reader: os.Stdin, Writer: os.Stdout, Closers: []io.Closer{os.Stdin, os.Stdout}}, version)
	case "rename-session":
		if len(args) != 4 && len(args) != 5 {
			return usage("rename-session requires old name, new name and client version")
		}
		if err := checkPeerVersion(args[3], "client"); err != nil {
			return err
		}
		return renameSession(args[1], args[2], args[4:]...)
	case "kill-session":
		if len(args) != 3 && len(args) != 4 {
			return usage("kill-session requires a name and client version")
		}
		if err := checkPeerVersion(args[2], "client"); err != nil {
			return err
		}
		return killSession(args[1], args[3:]...)
	case "bootstrap":
		if len(args) != 4 {
			return usage("invalid bootstrap arguments")
		}
		return bootstrap(args[1], args[2], args[3])
	case "bridge", "resume", "daemon":
		if args[0] == "daemon" && len(args) == 1 {
			return runSharedRemoteDaemon()
		}
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a shell name")
		}
		if args[0] == "bridge" || args[0] == "resume" {
			return bridge(args[1], args[0] == "resume")
		}
		return daemon(args[1])
	case "task-watch":
		if len(args) != 2 {
			return usage("invalid task watcher arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return watchRemoteTask()
	case "task-remove":
		if len(args) != 3 {
			return usage("invalid task removal arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return removeRemoteTask(args[2])
	case "inventory":
		if len(args) != 2 && (len(args) != 3 || args[2] != "--verbose") {
			return usage("inventory requires client version")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		inv, e := localSessionInventory(len(args) == 3)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(inv)
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
	case "forward":
		op, err := remoteForwardRequest(args[1:])
		if err != nil {
			return err
		}
		return requestOperations([]operation{op})
	case "detach":
		if len(args) != 1 {
			return usage("usage: sshore detach")
		}
		return detachHelper()
	case "cp":
		kind, paths, hint, err := remoteCopyRequest(args[1:])
		if err != nil {
			return err
		}
		return requestFilesWithHint(kind, paths, hint)
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
	deadline := time.Now().Add(2 * time.Second)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sock")
		if validSession(name) != nil {
			continue
		}
		if time.Until(deadline) <= 0 {
			return nil, fmt.Errorf("session lookup timed out")
		}
		c, err := net.DialTimeout("unix", filepath.Join(dir, e.Name()), min(time.Second, time.Until(deadline)))
		if err != nil {
			continue
		}
		probeDeadline := time.Now().Add(time.Second)
		if deadline.Before(probeDeadline) {
			probeDeadline = deadline
		}
		_ = c.SetDeadline(probeDeadline)
		err = writeMessage(c, message{Type: "status"})
		var m message
		if err == nil {
			m, err = readMessage(c)
		}
		c.Close()
		if err == nil && m.Type == "session" && !m.Attached && m.PID > 0 {
			names = append(names, sessionLabel(m, e.Name()))
			if len(names) > 128 {
				return nil, fmt.Errorf("too many shells to select; use sshore list HOST, then sshore attach HOST NAME")
			}
		}
	}
	return names, nil
}

func (s *shell) cancel(instance string) error {
	s.mu.Lock()
	if instance == "" || instance != s.instance {
		s.mu.Unlock()
		return fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	if s.cmd == nil {
		s.lifecycle.End()
		s.stop()
		s.mu.Unlock()
		return nil
	}
	s.cancelling = true
	pid, start := s.cmd.Process.Pid, s.processIdentity
	s.exitReason = "cancelled by sshore cancel"
	s.mu.Unlock()
	if err := stopOldSessions([]setupProcess{{PID: pid, Start: start}}, "cancelled by sshore cancel"); err != nil {
		s.mu.Lock()
		s.cancelling = false
		s.mu.Unlock()
		return err
	}
	s.stop()
	return nil
}
