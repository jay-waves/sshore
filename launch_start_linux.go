//go:build linux

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
			requested := publicLabel
			if fresh {
				requested = ""
			}
			namespace, err = reserveResourceName("shell", requested)
			if err != nil {
				if fresh && errors.Is(err, errObjectNameInUse) {
					continue
				}
				return sessionReservation{}, err
			}
		}
		if namespace != nil && fresh {
			name = namespace.uid
			path, err = socketPath(name)
			if err != nil {
				namespace.Close()
				return sessionReservation{}, err
			}
			publicLabel = namespace.name
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
	if !existing && freshOnly && isShortTaskID(name) {
		name = ""
	}
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
		if err := createRemoteShell(executable, name, publicName, reservation.namespace.uid, len(temporary) > 0 && temporary[0]); err != nil {
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

func detachHelper() error {
	sock := os.Getenv("SSHORE_SOCKET")
	if sock == "" {
		return fmt.Errorf("run this command inside an sshore shell")
	}
	c, err := net.DialTimeout("unix", sock, requestTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	if err := writeMessage(c, message{Type: "detach"}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "detached" {
		return fileResponseError(m)
	}
	return nil
}

func (s *remoteShell) sessionTargets(request message) message {
	s.mu.Lock()
	current, plain := s.name, s.plain
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: current}
	if plain {
		reply.Error = "session switching is disabled in recovery"
		return reply
	}
	if request.Action == "new" {
		occupied, err := occupiedObjectNames("")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		for _, name := range automaticSessionNames() {
			if !occupied[name] {
				reply.Names = []string{name}
				return reply
			}
		}
		reply.Error = "no automatic shell names available"
		return reply
	}
	names, err := detachedSessionNames()
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	if request.Action == "select" {
		for _, name := range names {
			if name == request.Name {
				reply.Names = []string{name}
				return reply
			}
		}
		reply.Error = fmt.Sprintf("shell %s is unavailable", cleanText(request.Name))
		return reply
	}
	reply.Names = names
	return reply
}
