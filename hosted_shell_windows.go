package main

import (
	"net"
	"time"
)

type remoteShell struct {
	shellTask
	tasks            *daemonTasks
	resourceApproval resourceApproval
}

func (s *remoteShell) serveTask(c net.Conn, m message) error {
	if handled, err := s.serveCommand(c, m, s.attachmentCommand, s.status); handled {
		return err
	}
	switch m.Type {
	case "route_mount", "route_forward", "route_files":
		s.serveRoutedResource(c, m, nil)
		return nil
	case "approval_request":
		if err := s.requestApproval(c, m); err != nil {
			return writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
		}
		return nil
	default:
		return writeMessage(c, message{Type: "error", Error: "unsupported Windows hosted shell operation"})
	}
}

func (s *remoteShell) attachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "sh_child":
		return p.controlMessage(message{Type: "sh_child_error", Error: "Windows hosted shells do not support nested routes"})
	default:
		return attachmentApprovalCommand(p, m)
	}
}

func (s *remoteShell) hostedView(_ string) shellView {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := shellView{
		Owner: s.parentIdentityLocked(), Pinned: s.pinned, PinUntil: s.pinUntil,
		Instance: s.record.UID, ID: "sh/" + s.record.Name, Name: s.record.Name,
		PID: s.pid, Shell: s.shellName, State: s.state(),
		Directory: s.directoryOrStartupLocked(),
	}
	if s.active != nil {
		view.SourceHost = s.active.sourceHost
	}
	if view.PID != 0 {
		age := int64(time.Since(s.started) / time.Second)
		view.Age = &age
	}
	return view
}
