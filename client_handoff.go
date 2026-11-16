package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/term"
)

// Only local authenticated control carries this request; terminal output never does.
type clientHandoff struct {
	Args                   []string
	Owner                  spawnOwner
	Directory              string
	Environment            []string
	Tree                   []shellTreeNode
	TaskIdentity, Instance string
}

func (h *clientHandoff) options() (options, error) {
	if h == nil {
		return options{}, fmt.Errorf("missing handoff request")
	}
	o, err := parseOptions(h.Args)
	if err != nil {
		return o, err
	}
	if o.detached || (o.command != "spawn" && o.command != "launch" && o.command != "attach") || !filepath.IsAbs(h.Directory) {
		return o, fmt.Errorf("invalid remote handoff")
	}
	if o.command == "spawn" {
		identity, err := spawnProcessIdentity(h.Owner.PID)
		if h.Owner.PID <= 1 || err != nil || identity != h.Owner.Start {
			return o, fmt.Errorf("spawn owner exited")
		}
		o.temporary, o.owner = true, h.Owner
	}
	o.sshDirectory, o.environment, o.tree = h.Directory, h.Environment, h.Tree
	o.taskIdentity, o.instance = h.TaskIdentity, h.Instance
	return o, nil
}

func handoffLocalClient(o options) (bool, error) {
	identity := os.Getenv("SSHORE_LOCAL_SESSION")
	if identity == "" || o.detached || (o.command != "spawn" && o.command != "launch" && o.command != "attach") || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true, err
	}
	if o.sshDirectory != "" {
		cwd = o.sshDirectory
	}
	args := []string{o.command}
	if o.local {
		args = append(args, "--local")
	} else {
		args = append(args, o.host)
	}
	if o.session != "" {
		args = append(args, o.session)
	}
	if !o.local {
		args = append(args, "--server", o.server)
	}
	if o.downloadDir != "" {
		args = append(args, "--download-dir", o.downloadDir)
	}
	for _, flag := range []struct {
		name    string
		enabled bool
	}{{"--quiet", o.quiet}, {"--no-escape", o.noEscape}, {"--no-login", o.noLogin}, {"--no-profile", o.noProfile}} {
		if flag.enabled {
			args = append(args, flag.name)
		}
	}
	if len(o.ssh) > 0 {
		args = append(append(args, "--"), o.ssh...)
	}
	environment := os.Environ()
	if !o.local {
		// Resolve the initiating shell's executable before handing off.
		ssh, err := exec.LookPath(sshExecutable())
		if err != nil {
			return true, fmt.Errorf("system OpenSSH: %w", err)
		}
		ssh, err = filepath.Abs(ssh)
		if err != nil {
			return true, err
		}
		environment = append(environment, "SSHORE_SSH="+ssh)
	}
	h := &clientHandoff{Args: args, Owner: o.owner, Directory: cwd, Environment: environment, Tree: currentShellTree(), TaskIdentity: o.taskIdentity, Instance: o.instance}
	rows, err := localShells()
	if err != nil {
		return true, err
	}
	for _, row := range rows {
		if row.identity != identity {
			continue
		}
		m, err := localRequest(row.Name, message{Type: "handoff", Handoff: h}, identity)
		if err != nil {
			return true, err
		}
		if m.Type != "handoff_accepted" {
			return true, fmt.Errorf("remote handoff: %s", m.Error)
		}
		return true, nil
	}
	return true, fmt.Errorf("local shell is unavailable for remote handoff")
}

type localHandoff struct {
	token      string
	attachment *attachment
	result     chan message
}

func (s *localShell) requestHandoff(c net.Conn, h *clientHandoff) error {
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := h.options(); err != nil {
		return writeMessage(c, message{Type: "error", Error: err.Error()})
	}
	s.mu.Lock()
	p := s.lifecycle.Active()
	if p == nil || s.handoff != nil {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "local frontend unavailable or busy"})
	}
	pending := &localHandoff{token: resourceToken(), attachment: p, result: make(chan message, 1)}
	s.handoff = pending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.handoff == pending {
			s.handoff = nil
		}
		s.mu.Unlock()
	}()
	if err := p.controlMessage(message{Type: "handoff", Handoff: h, Token: pending.token}); err != nil {
		return err
	}
	select {
	case result := <-pending.result:
		if result.Error != "" {
			return writeMessage(c, message{Type: "error", Error: result.Error})
		}
		if err := p.controlMessage(message{Type: "handoff_commit", Token: pending.token}); err != nil {
			return err
		}
		select {
		case result := <-pending.result:
			if result.Error != "" {
				return writeMessage(c, message{Type: "error", Error: result.Error})
			}
			return writeMessage(c, message{Type: "handoff_accepted"})
		case <-time.After(2 * time.Minute):
			return writeMessage(c, message{Type: "error", Error: "attachment startup timed out"})
		}
	case <-p.Mux().CloseChan():
		return writeMessage(c, message{Type: "error", Error: "local frontend disconnected"})
	case <-time.After(4 * time.Second):
		return writeMessage(c, message{Type: "error", Error: "local frontend handoff timed out"})
	}
}

func (s *localShell) finishHandoff(p *attachment, m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.handoff; h != nil && h.attachment == p && h.token == m.Token {
		select {
		case h.result <- m:
		default:
		}
	}
}

func (s *localShell) confirmHandoff(c net.Conn, m message) error {
	s.mu.Lock()
	pending := s.handoff
	if pending == nil || pending.token != m.Token {
		s.mu.Unlock()
		return writeMessage(c, message{Type: "error", Error: "handoff expired"})
	}
	select {
	case pending.result <- m:
	default:
	}
	s.mu.Unlock()
	return writeMessage(c, message{Type: "handoff_confirmed"})
}
