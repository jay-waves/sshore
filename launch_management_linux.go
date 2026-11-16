//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
