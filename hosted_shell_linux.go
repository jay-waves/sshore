//go:build linux

package main

import (
	"net"

	"github.com/jay-waves/sshore/internal/sys"
)

type hostedRoutes struct {
	shellRoutes map[string]*shellRoute
}

func (s *remoteShell) attachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "sh_child":
		s.mu.Lock()
		req := &localDaemonRequest{Record: localShellRecord{Directory: s.directoryOrStartupLocked()}, Env: append([]string(nil), s.environment...), Shell: &shellConfig{Command: "sh", Local: true, Pin: m.Pinned}}
		s.mu.Unlock()
		if m.Directory != "" {
			req.Record.Directory = m.Directory
		}
		go func() { p.childResult(s.localShellRoute(nil, p, *req)) }()
	default:
		return attachmentApprovalCommand(p, m)
	}
	return nil
}

func (s *remoteShell) serveTask(c net.Conn, first message) error {
	if first.Type == "shell_owner_access" {
		owned := &ownedShellConnection{Conn: c, token: first.Token}
		s.mu.Lock()
		allowed := s.remoteOwner != "" && s.checkRemoteOwnerLocked(owned) == nil
		s.mu.Unlock()
		if !allowed || first.SoftwareVersion != version || first.Instance != s.record.UID {
			return writeMessage(c, message{Type: "error", Error: "shell owner route unavailable"})
		}
		if err := writeMessage(c, message{Type: "shell_owner_access_ready", SoftwareVersion: version}); err != nil {
			return err
		}
		var err error
		first, err = readMessage(c)
		if err != nil {
			return err
		}
		c = owned
	}
	if handled, err := s.serveHostedTask(c, first, s.relayResourceRoute); handled {
		return err
	}
	switch first.Type {
	case "shell_connect":
		return s.connectRouteShell(c, first)
	case "shell_route_confirm":
		s.confirmShellRoute(c, first)
	case "shell_route_request":
		s.requestShellRoute(c, first)
	case "shell_route_connect":
		s.connectShellRoute(c, first)
	default:
		return writeMessage(c, message{Type: "fatal", Error: "unsupported connection; update sshore on both computers"})
	}
	return nil
}

func (s *remoteShell) hostedView(detail string) shellView {
	view := s.hostedSnapshot(false)
	var group int
	var processes []sys.Process
	if view.PID != 0 && (detail == "" || detail == "sessions") {
		group, processes = s.runtime.Processes()
	}
	hostedDiagnostics(&view, processes, group, detail)
	return view
}
