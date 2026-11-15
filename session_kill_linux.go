package main

// Explicit kill resolves a name, or an account-wide confirmed instance, while holding
// the account setup lock. Only a discovered same-user daemon whose socket peer
// matches its PID is eligible. Reuse setup's process snapshot and pidfd signals
// (TERM, then bounded KILL) so PID reuse cannot redirect termination. No setup,
// installation, version migration, or name-wide signaling occurs on this path.
import (
	"fmt"
	"net"
	"os"
	"time"
)

func killSession(name string, expected ...string) error {
	if err := validSession(name); err != nil {
		return err
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return err
	}
	var path string
	if len(expected) > 0 && expected[0] != "" {
		// Inventory spans runtime directories. Resolve the confirmed instance,
		// never a same-name session in the current directory.
		for _, candidate := range processes {
			if candidate.socket == "" {
				continue
			}
			m, err := sessionStatus(candidate.socket)
			if err == nil && m.Instance == expected[0] {
				if path != "" && path != candidate.socket {
					return fmt.Errorf("ambiguous shell instance; refresh with sshore list")
				}
				path = candidate.socket
			}
		}
	} else {
		path, err = resolveSession(name)
		if err != nil {
			return err
		}
	}
	if path == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	var matches []daemonProcess
	for _, p := range processes {
		if p.socket == path {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("shell %s: expected one identifiable daemon, found %d; use sshore list", name, len(matches))
	}
	p := matches[0]
	start, err := processStart(p.pid)
	if err != nil {
		return err
	}
	v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", p.pid))
	if err != nil {
		return fmt.Errorf("cannot verify shell version: %w", err)
	}
	if err := checkPeerVersion(v, "shell daemon"); err != nil {
		return err
	}
	if len(expected) > 0 && expected[0] != "" {
		m, err := sessionStatus(path)
		if err != nil {
			return err
		}
		if m.Instance != expected[0] {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
	}
	health := probeSessionDaemon(p, time.Now().Add(time.Second))
	if health.state != "attached" && health.state != "detached" && health.state != "starting" {
		return fmt.Errorf("shell %s is %s; refusing to signal an unconfirmed daemon", name, health.state)
	}
	if p.shared {
		m, err := sessionStatus(path)
		if err != nil {
			return err
		}
		if len(expected) > 0 && expected[0] != "" && m.Instance != expected[0] {
			return fmt.Errorf("shell instance changed; refresh with sshore list")
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err = writeMessage(c, message{Type: "cancel", Instance: m.Instance, SoftwareVersion: version}); err != nil {
			return err
		}
		reply, err := readMessage(c)
		if err != nil {
			return err
		}
		if reply.Type != "cancelled" {
			return fmt.Errorf("cancel shell: %s", reply.Error)
		}
		fmt.Fprintf(os.Stdout, "Ended shell %s\n", name)
		return nil
	}
	if err := stopOldSessions([]setupProcess{{PID: p.pid, Start: start, Version: v}}, "cancelled by sshore cancel"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Ended shell %s\n", name)
	return nil
}
