package main

import "net"

type hostedRoutes struct{}

func (s *tmuxTask) serveHostedIngress(c net.Conn, m message) error {
	if handled, err := s.serveHostedTask(c, m, nil); handled {
		return err
	}
	return writeMessage(c, message{Type: "error", Error: "unsupported Windows hosted shell operation"})
}

func (s *tmuxTask) hostedAttachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "sh_child":
		return p.controlMessage(message{Type: "sh_child_error", Error: "Windows hosted shells do not support nested routes"})
	default:
		return attachmentApprovalCommand(p, m)
	}
}
