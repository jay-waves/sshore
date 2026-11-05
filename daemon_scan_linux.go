package main

// A detached daemon is expected, not a leak. Inspect only this user's session
// daemons, and require a responding socket owned by that PID before calling one
// healthy. Failed probes are suspects, never grounds for automatic cleanup.
import (
	"debug/buildinfo"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type daemonProcess struct {
	pid          int
	name, socket string
}

type daemonHealth struct {
	process daemonProcess
	state   string
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
		if len(args) != 3 || args[1] != "daemon" || validSession(args[2]) != nil {
			continue
		}
		if !same && name != "sshore" && !strings.HasPrefix(name, "sshore-") {
			build, err := buildinfo.ReadFile(filepath.Join(base, "exe"))
			if err != nil || build.Main.Path != "github.com/jay-waves/sshore" {
				continue
			}
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
	result.state = "detached"
	if m.PID == 0 {
		result.state = "starting"
	} else if m.Attached {
		result.state = "attached"
	}
	return result
}

func reportSessionDaemons() {
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		logf(os.Stderr, logWarning, "[sshore] Remote session daemon scan unavailable: %s\n", cleanText(err.Error()))
		return
	}
	// A single time budget bounds the connection delay even with stuck daemons.
	deadline := time.Now().Add(500 * time.Millisecond)
	results := make([]daemonHealth, len(processes))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(8, len(processes)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				results[i] = probeSessionDaemon(processes[i], deadline)
			}
		}()
	}
	for i := range processes {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	counts := make(map[string]int)
	var suspects []string
	for _, result := range results {
		counts[result.state]++
		if result.state == "suspect" && len(suspects) < 10 {
			suspects = append(suspects, fmt.Sprintf("%s (PID %d)", result.process.name, result.process.pid))
		}
	}
	logf(os.Stderr, logInfo, "[sshore] Remote session daemons (your user, visible in /proc): %d total; %d attached, %d detached, %d starting, %d suspect, %d unverified.\n", len(processes), counts["attached"], counts["detached"], counts["starting"], counts["suspect"], counts["unverified"])
	if len(suspects) > 0 {
		logf(os.Stderr, logWarning, "[sshore] Possible stale or unresponsive daemons (up to 10): %s. This is a snapshot, not proof of a leak; no processes were stopped.\n", strings.Join(suspects, ", "))
	}
}
