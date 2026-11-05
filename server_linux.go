//go:build linux

package main

// Each named session has one daemon and PTY; there is no global daemon or extra
// listening network port. bridge connects SSH stdin/stdout to the Unix socket,
// starting the daemon as needed. The daemon drains and discards detached PTY
// output so an unread terminal cannot block remote programs.
//
// Runtime files: .sock is the communication endpoint; .lock and .start coordinate
// lifetime and startup; .log records background errors. Locks and logs can outlive
// a session. Listing queries sockets rather than treating leftover files as live
// sessions. The shell receives SSHORE_SOCKET for remote helper commands.

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

// Automatic allocation holds the same per-name startup lock as explicit attach.
// Keep that lock until the daemon listens, so simultaneous clients cannot claim
// one name. Live detached sessions also reserve their names; stale sockets do not.
var sessionNames = strings.Fields(`
pikachu eevee bulbasaur charmander squirtle snorlax psyduck meowth gengar lapras
 ditto mew togepi pichu vulpix jigglypuff ivysaur venusaur charmeleon charizard
 wartortle blastoise caterpie metapod butterfree weedle kakuna beedrill pidgey
 pidgeotto pidgeot rattata raticate spearow fearow ekans arbok raichu sandshrew
 sandslash clefairy clefable ninetales wigglytuff zubat golbat oddish gloom
 vileplume paras parasect venonat venomoth diglett dugtrio persian golduck mankey
 primeape growlithe arcanine poliwag poliwhirl poliwrath abra kadabra alakazam
 machop machoke machamp bellsprout weepinbell victreebel tentacool tentacruel
 geodude graveler golem ponyta rapidash slowpoke slowbro magnemite magneton
 farfetchd doduo dodrio seel dewgong grimer muk shellder cloyster gastly haunter
 onix drowzee hypno krabby kingler voltorb electrode exeggcute exeggutor cubone
 marowak hitmonlee hitmonchan lickitung koffing weezing rhyhorn rhydon chansey
 tangela kangaskhan horsea seadra goldeen seaking staryu starmie scyther jynx
 electabuzz magmar pinsir tauros magikarp gyarados vaporeon jolteon flareon
 porygon omanyte omastar kabuto kabutops aerodactyl articuno zapdos moltres
 dratini dragonair dragonite mewtwo chikorita cyndaquil totodile sentret furret
 hoothoot noctowl mareep flaaffy ampharos marill azumarill espeon umbreon
`)

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

func bridge(name string) error {
	names := []string{name}
	fresh := name == ""
	if fresh {
		names = sessionNames
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
		exe, err := os.Executable()
		if err != nil {
			return err
		}
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
}

func (p *attachment) controlMessage(m message) error {
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	return streamMessage(p.control, m)
}
func (p *attachment) output(m message) error { return streamMessage(p.terminal, m) }
func (p *attachment) close()                 { _ = p.mux.Close() }

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
	mu        sync.Mutex
	active    *attachment
	transfers map[uint64]*activeTransfer
	master    *os.File
	cmd       *exec.Cmd
	exited    bool
	path      string
	done      chan struct{}
	once      sync.Once
}

func (s *session) stop() { s.once.Do(func() { close(s.done) }) }
func (s *session) start(cols, rows uint16, term string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-i")
	cmd.Env = append(os.Environ(), "TERM="+term, "SSHORE_SOCKET="+s.path)
	if exe, err := os.Executable(); err == nil {
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
				s.mu.Lock()
				p := s.active
				s.mu.Unlock()
				if p != nil {
					if p.output(message{Type: "output", Data: buf[:n]}) != nil {
						p.close()
					}
				}
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
	if m.Type != "attach" || m.Version != protocolVersion {
		return fmt.Errorf("protocol version mismatch")
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
	if s.master == nil {
		if err := s.start(m.Cols, m.Rows, m.Term); err != nil {
			return err
		}
	}
	if err := pty.Setsize(s.master, &pty.Winsize{Cols: max(m.Cols, 1), Rows: max(m.Rows, 1)}); err != nil {
		return err
	}
	if err := p.controlMessage(message{Type: "ready", Version: protocolVersion, PID: s.cmd.Process.Pid, Name: strings.TrimSuffix(filepath.Base(s.path), ".sock")}); err != nil {
		return err
	}
	s.active = p
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
func (s *session) serveMux(c net.Conn) error {
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
	p := &attachment{mux: mux, control: control, terminal: terminal}
	defer s.release(p)
	if err = s.attach(p, m); err != nil {
		_ = p.controlMessage(message{Type: "fatal", Error: err.Error()})
		_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = readMessage(control)
		return err
	}
	// No further client-initiated streams are part of the protocol. Reject them
	// rather than allowing unread streams to consume the shared receive budget.
	go func() {
		for {
			st, err := mux.AcceptStream()
			if err != nil {
				return
			}
			_ = st.Close()
		}
	}()
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
		if first.Version != protocolVersion {
			_ = writeMessage(c, message{Type: "fatal", Error: "protocol version mismatch; update both sshore binaries"})
			return
		}
		if writeMessage(c, message{Type: "mux_ready", Version: protocolVersion}) == nil {
			_ = s.serveMux(c)
		}
	case "status":
		s.mu.Lock()
		m := message{Type: "session", Attached: s.active != nil}
		if s.cmd != nil {
			m.PID = s.cmd.Process.Pid
		}
		s.mu.Unlock()
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
	case "upload", "offer":
		if err = s.transfer(id, c, first); err != nil {
			_ = writeMessage(c, message{Type: "fatal", Error: err.Error()})
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
	fmt.Println("SESSION\tPID\tSTATE")
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
		fmt.Printf("%s\t%d\t%s\n", strings.TrimSuffix(e.Name(), ".sock"), m.PID, state)
	}
	return nil
}
func serverCommand(args []string) error {
	switch args[0] {
	case "bridge", "daemon":
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a session name")
		}
		if args[0] == "bridge" {
			return bridge(args[1])
		}
		return daemon(args[1])
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
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
		return helper("upload", path)
	case "download":
		if len(args) < 2 {
			return usage("usage: sshore download FILE...")
		}
		return downloadFiles(args[1:])
	}
	return fmt.Errorf("unknown server command")
}
