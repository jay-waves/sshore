//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
)

type remoteShell struct {
	shellTask
	tasks            *daemonTasks
	shellRoutes      map[string]*shellRoute
	resourceApproval resourceApproval
}

func (s *remoteShell) attachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "sh_child":
		s.mu.Lock()
		req := &localDaemonRequest{Record: localShellRecord{Directory: s.directoryOrStartupLocked()}, Env: append([]string(nil), s.environment...), Shell: &shellConfig{Command: "sh", Local: true, Pin: m.Pinned}}
		s.mu.Unlock()
		go serveChildShortcut(p, s.done, func(c net.Conn) {
			s.requestShellRoute(c, message{SoftwareVersion: version, LocalRequest: req})
		})
	case "approval_ready":
		p.approvals.enable()
	case "approval_handoff", "approval_rejected":
		p.approvals.reply(m)
	default:
		return fmt.Errorf("unexpected control message: %s", m.Type)
	}
	return nil
}

func (s *remoteShell) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	first, err := readMessage(c)
	if err != nil {
		return
	}
	s.handleTask(c, first)
}

func (s *remoteShell) handleTask(c net.Conn, first message) {
	if first.Type == "shell_owner_access" {
		owned := &ownedShellConnection{Conn: c, token: first.Token}
		s.mu.Lock()
		allowed := s.remoteOwner != "" && s.checkRemoteOwnerLocked(owned) == nil
		s.mu.Unlock()
		if !allowed || first.SoftwareVersion != version || first.Instance != s.record.UID {
			_ = writeMessage(c, message{Type: "error", Error: "shell owner route unavailable"})
			return
		}
		if err := writeMessage(c, message{Type: "shell_owner_access_ready", SoftwareVersion: version}); err != nil {
			return
		}
		var err error
		first, err = readMessage(c)
		if err != nil {
			return
		}
		c = owned
	}
	if handled, _ := s.serveCommand(c, first, s.attachmentCommand, s.status); handled {
		return
	}
	switch first.Type {
	case "route_mount", "route_forward", "route_files":
		s.serveRoutedResource(c, first)
	case "shell_connect":
		_ = s.connectRouteShell(c, first)
	case "shell_route_confirm":
		s.confirmShellRoute(c, first)
	case "shell_route_request":
		s.requestShellRoute(c, first)
	case "shell_route_connect":
		s.connectShellRoute(c, first)
	case "approval_request":
		if err := s.requestApproval(c, first); err != nil {
			_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
		}
	default:
		_ = writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
}
func (s *remoteShell) hostedView(detail string) shellView {
	s.mu.Lock()
	view := shellView{
		Owner: s.parentIdentityLocked(), Pinned: s.pinned, PinUntil: s.pinUntil, Instance: s.record.UID,
		ID: "sh/" + s.record.Name, Name: s.record.Name, PID: s.pid, Shell: s.shellName,
	}
	view.State = s.state()
	if s.active != nil {
		view.SourceHost = s.active.sourceHost
	}
	if view.PID != 0 {
		age := int64(time.Since(s.started) / time.Second)
		view.Age = &age
	}
	terminal := s.terminal
	s.mu.Unlock()
	var group int
	var processes []sys.Process
	if terminal != nil && (detail == "" || detail == "sessions") {
		group, _ = terminal.ForegroundGroup()
		processes = terminal.TaskProcesses()
	}
	hostedDiagnostics(&view, processes, group, detail)
	return view
}

func (s *remoteShell) serveTask(c net.Conn, m message) error {
	s.handleTask(c, m)
	return nil
}

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func resolveSession(name string, expected ...string) (string, error) {
	task, err := resolveHostedShell(name, firstExpectedInstance(expected))
	return task.Instance, err
}

func renameSession(old, name string, expected ...string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	task, err := resolveHostedShell(old, firstExpectedInstance(expected))
	if err != nil {
		return err
	}
	if task.Instance == "" {
		return fmt.Errorf("shell %s does not exist", old)
	}

	// The daemon rechecks the selected label and UID when it changes the name.
	c, err := dialHostedTask(context.Background(), task.Instance, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "rename", Instance: task.Instance, Name: name, Action: task.Name, SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "renamed" {
		return fmt.Errorf("rename: %s", m.Error)
	}
	fmt.Fprintf(os.Stdout, "Renamed shell %s to %s\n", task.Name, name)
	return nil
}

func firstExpectedInstance(expected []string) string {
	if len(expected) > 0 {
		return expected[0]
	}
	return ""
}

// Cancellation is authorized and applied by the task owner. Process identity
// validation belongs to the daemon immediately before it signals its child.
func killSession(name string, expected ...string) error {
	id, err := resolveSession(name, expected...)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	c, err := dialHostedTask(context.Background(), id, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "kill", Instance: id, SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "killed" {
		return fmt.Errorf("kill shell: %s", reply.Error)
	}
	fmt.Fprintf(os.Stdout, "Ended shell %s\n", name)
	return nil
}
