package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/mcp"
	"golang.org/x/sys/unix"
)

// A detached daemon is expected, not a leak. Inspect only this user's visible
// session daemons across runtime directories. A healthy daemon requires a
// responding socket owned by its PID. Reports distinguish attached, detached,
// starting, suspect and unverified processes; an unreachable socket or mismatched
// owner is a suspect, never grounds for automatic cleanup. Socket probes share a
// 500 ms budget. This is a best-effort /proc snapshot: permissions and concurrent
// startup/exit affect visibility. Diagnostics never terminate processes; confirmed
// setup cleanup is implemented separately in setup_linux.go.

type daemonProcess struct {
	shared       bool
	pid          int
	name, socket string
}

type daemonHealth struct {
	process daemonProcess
	state   string
	name    string
}

func daemonSocket(name string, env []byte, uid uint32) string {
	values := make(map[string]string)
	for _, item := range strings.Split(string(env), "\x00") {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	dir := values["SSHORE_RUNTIME_DIR"]
	if dir == "" {
		if root := values["XDG_RUNTIME_DIR"]; root != "" {
			dir = filepath.Join(root, "sshore")
		} else {
			dir = fmt.Sprintf("/tmp/sshore-%d", uid)
		}
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, name+".sock")
}

func findSessionDaemons(root string) ([]daemonProcess, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	self, _ := os.Stat("/proc/self/exe")
	var found []daemonProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		base := filepath.Join(root, entry.Name())
		info, err := os.Stat(base)
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			continue
		}
		exe, err := os.Readlink(filepath.Join(base, "exe"))
		if err != nil {
			continue
		}
		name := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
		same := false
		if other, e := os.Stat(filepath.Join(base, "exe")); e == nil && self != nil {
			same = os.SameFile(self, other)
		}
		cmd, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(cmd), "\x00"), "\x00")
		shared := len(args) == 2 && args[1] == "daemon"
		if !shared && (len(args) != 3 || args[1] != "daemon" || validSession(args[2]) != nil) {
			continue
		}
		if !same && name != "sshore" && !strings.HasPrefix(name, "sshore-") {
			build, err := buildinfo.ReadFile(filepath.Join(base, "exe"))
			if err != nil || build.Main.Path != "github.com/jay-waves/sshore" {
				continue
			}
		}
		if shared {
			env, err := os.ReadFile(filepath.Join(base, "environ"))
			if err != nil {
				candidate := daemonProcess{pid: pid, name: "daemon", shared: true}
				if includeSetupProcess(candidate) {
					found = append(found, candidate)
				}
				continue
			}
			dir := filepath.Dir(daemonSocket("daemon", env, stat.Uid))
			entries, err := os.ReadDir(dir)
			if err != nil {
				candidate := daemonProcess{pid: pid, name: "daemon", shared: true}
				if includeSetupProcess(candidate) {
					found = append(found, candidate)
				}
				continue
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".sock.lock") {
					continue
				}
				socket := filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".lock"))
				if live, err := sessionIdentityLive(socket); err != nil || !live {
					continue
				}
				process := daemonProcess{pid: pid, name: strings.TrimSuffix(filepath.Base(socket), ".sock"), socket: socket, shared: true}
				// Legacy daemons may coexist in this directory; only this socket's owner belongs here.
				if owner := sessionSocketPID(socket); owner != 0 && owner != pid {
					continue
				}
				if includeSetupProcess(process) {
					found = append(found, process)
				}
			}
			continue
		}
		process := daemonProcess{pid: pid, name: args[2]}
		if env, err := os.ReadFile(filepath.Join(base, "environ")); err == nil {
			process.socket = daemonSocket(process.name, env, stat.Uid)
		}
		if includeSetupProcess(process) {
			found = append(found, process)
		}
	}
	return found, nil
}

func probeSessionDaemon(p daemonProcess, deadline time.Time) daemonHealth {
	result := daemonHealth{process: p, state: "unverified"}
	if p.socket == "" || time.Now().After(deadline) {
		return result
	}
	c, err := net.DialTimeout("unix", p.socket, time.Until(deadline))
	if err != nil {
		result.state = "suspect"
		return result
	}
	defer c.Close()
	_ = c.SetDeadline(deadline)
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return result
	}
	var peer *unix.Ucred
	var credentialErr error
	if err = raw.Control(func(fd uintptr) {
		peer, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credentialErr != nil {
		return result
	}
	if int(peer.Pid) != p.pid || peer.Uid != uint32(os.Geteuid()) {
		result.state = "suspect"
		return result
	}
	if err := writeMessage(c, message{Type: "status", Action: "health"}); err != nil {
		result.state = "suspect"
		return result
	}
	m, err := readMessage(c)
	if err != nil || m.Type != "session" {
		result.state = "suspect"
		return result
	}
	result.name = m.Name
	result.state = "detached"
	if m.PID == 0 {
		result.state = "starting"
	} else if m.Attached {
		result.state = "attached"
	}
	return result
}

func sessionSocketPID(path string) int {
	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return 0
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		peer, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err == nil && peer.Uid == uint32(os.Geteuid()) {
			pid = int(peer.Pid)
		}
	})
	return pid
}

func remoteDaemonPath() (string, error) {
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.control"), nil
}

// The caller holds the account setup gate, serializing daemon launch and updates.
func connectRemoteDaemon(executable string) (net.Conn, error) {
	path, err := remoteDaemonPath()
	if err != nil {
		return nil, err
	}
	connect := func() (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) }
	c, err := connect()
	if err != nil {
		// The last shell may have closed the listener just before its owner lock
		// is released. Never replace a live owner or race its final cleanup.
		until := time.Now().Add(3 * time.Second)
		for {
			owner, lockErr := openLock(path+".lock", true)
			if lockErr == nil {
				owner.Close()
				break
			}
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) || time.Now().After(until) {
				return nil, fmt.Errorf("remote daemon unavailable: %w", lockErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		log, err := os.OpenFile(path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err = privateStatePath(path+".log", false); err != nil {
			log.Close()
			return nil, err
		}
		cmd := exec.Command(executable, "daemon")
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = cmd.Start()
		log.Close()
		if err != nil {
			return nil, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
	wait:
		for {
			select {
			case err := <-done:
				return nil, fmt.Errorf("remote daemon exited: %v; see %s.log", err, path)
			case <-deadline.C:
				return nil, fmt.Errorf("remote daemon startup timed out; see %s.log", path)
			case <-tick.C:
				c, err = connect()
				if err == nil {
					break wait
				}
			}
		}
	}
	return c, nil
}

func createRemoteShell(executable, name, label, uid string, temporary ...bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return createRemoteShellUsing(executable, name, label, uid, len(temporary) > 0 && temporary[0], cwd, os.Environ())
}

func createRemoteShellUsing(executable, name, label, uid string, temporary bool, cwd string, env []string) error {
	c, err := connectRemoteDaemon(executable)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	request := message{Type: "create_shell", Instance: uid, Temporary: temporary, SoftwareVersion: version, Name: name, Action: label, LocalRequest: &localDaemonRequest{Record: localShellRecord{Directory: cwd}, Env: env}}
	if err = writeMessage(c, request); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "shell_created" || reply.SoftwareVersion != version {
		return fmt.Errorf("remote shell creation failed: %s", reply.Error)
	}
	return nil
}

func runSharedRemoteDaemon() error { return runSharedRemoteDaemonUsing(checkMCPSessionSetup) }

func runSharedRemoteDaemonUsing(check func() error) error {
	path, err := remoteDaemonPath()
	if err != nil {
		return err
	}
	lock, err := openLock(path+".lock", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var mu sync.Mutex
	active := 0
	closing := false
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-done:
		}
	}()
	// An abandoned launcher must not leave an empty service behind.
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			mu.Lock()
			if active == 0 {
				closing = true
				listener.Close()
			}
			mu.Unlock()
		case <-done:
		}
	}()
	var shells sync.WaitGroup
	mcpSessions := newTrackedMCPSessions(check)
	defer func() {
		mcpSessions.closeAll()
		shells.Wait()
		mcpSessions.closeAll()
	}()
	for {
		c, err := listener.Accept()
		if err != nil {
			return nil
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := readMessage(c)
		if err != nil {
			c.Close()
			continue
		}
		fail := func(err error) {
			_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
			c.Close()
		}
		if err = checkPeerVersion(request.SoftwareVersion, "client"); err != nil {
			fail(err)
			continue
		}
		if request.Type == "mcp_request" && request.MCPRequest != nil {
			mu.Lock()
			if closing {
				mu.Unlock()
				fail(fmt.Errorf("daemon is exiting; retry"))
				continue
			}
			active++
			mu.Unlock()
			shells.Add(1)
			go func() {
				defer shells.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(40 * time.Second))
				response := mcp.Dispatch(ctx, mcpSessions, *request.MCPRequest, version)
				mu.Lock()
				if response.Error == "" {
					if request.MCPRequest.Method == "open" {
						active++
					}
					if request.MCPRequest.Method == "close" {
						active--
					}
				}
				active--
				if active == 0 {
					closing = true
					listener.Close()
				}
				mu.Unlock()
				_ = writeMessage(c, message{Type: "mcp_response", SoftwareVersion: version, MCPResponse: &response})
			}()
			continue
		}
		if request.Type != "create_shell" || request.LocalRequest == nil || !validTaskUID(request.Instance) || validSession(request.Name) != nil || validSession(request.Action) != nil || !filepath.IsAbs(request.LocalRequest.Record.Directory) {
			fail(fmt.Errorf("invalid shell creation"))
			continue
		}
		mu.Lock()
		if closing {
			mu.Unlock()
			fail(fmt.Errorf("daemon is exiting; retry"))
			continue
		}
		active++
		mu.Unlock()
		shells.Add(1)
		go func() {
			defer shells.Done()
			defer c.Close()
			defer func() {
				mu.Lock()
				active--
				if active == 0 {
					closing = true
					listener.Close()
				}
				mu.Unlock()
			}()
			ready := false
			err := serveRemoteShell(request.Name, request.Action, request.Instance, request.LocalRequest.Env, request.LocalRequest.Record.Directory, func() error {
				err := writeMessage(c, message{Type: "shell_created", SoftwareVersion: version})
				if err == nil {
					ready = true
				}
				c.Close()
				return err
			}, request.Temporary)
			if err != nil && !ready {
				_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
			}
		}()
	}
}
