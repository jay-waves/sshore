package main

import (
	"fmt"
	"net"
	"time"
)

type remoteShell struct {
	shellTask
	tasks *daemonTasks
}

func (s *remoteShell) serveTask(c net.Conn, m message) error {
	if handled, err := s.serveCommand(c, m, s.attachmentCommand, s.status); handled {
		return err
	}
	return writeMessage(c, message{Type: "error", Error: "unsupported Windows hosted shell operation"})
}

func (s *remoteShell) attachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "approval_ready":
		return nil
	case "sh_child":
		return p.controlMessage(message{Type: "sh_child_error", Error: "Windows hosted shells do not support nested routes"})
	default:
		return fmt.Errorf("unexpected control message: %s", m.Type)
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
