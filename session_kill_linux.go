package main

// Explicit kill resolves one name in the current runtime directory while holding
// the account setup lock. Only a discovered same-user daemon whose socket peer
// matches its PID is eligible. Reuse setup's process snapshot and pidfd signals
// (TERM, then bounded KILL) so PID reuse cannot redirect termination. No setup,
// installation, version migration, or name-wide signaling occurs on this path.
import (
	"fmt"
	"os"
	"time"
)

func killSession(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	path, err := resolveSession(name)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("session %s does not exist", name)
	}
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return err
	}
	var matches []daemonProcess
	for _, p := range processes {
		if p.socket == path {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("session %s: expected one identifiable daemon, found %d; use sshore list", name, len(matches))
	}
	p := matches[0]
	start, err := processStart(p.pid)
	if err != nil {
		return err
	}
	v, err := executableVersion(fmt.Sprintf("/proc/%d/exe", p.pid))
	if err != nil {
		return fmt.Errorf("cannot verify session version: %w", err)
	}
	if err := checkPeerVersion(v, "session daemon"); err != nil {
		return err
	}
	health := probeSessionDaemon(p, time.Now().Add(time.Second))
	if health.state != "attached" && health.state != "detached" && health.state != "starting" {
		return fmt.Errorf("session %s is %s; refusing to signal an unconfirmed daemon", name, health.state)
	}
	if err := stopOldSessions([]setupProcess{{PID: p.pid, Start: start, Version: v}}, "cancelled by sshore cancel"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Ended session %s\n", name)
	return nil
}
