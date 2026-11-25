//go:build linux

package main

import (
	"net"
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
	default:
		return attachmentApprovalCommand(p, m)
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
		s.serveRoutedResource(c, first, s.relayResourceRoute)
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
