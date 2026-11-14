package main

// Local requests default to independent background workers; --foreground is
// available for diagnostics. Remote shell
// requests inherit the session and require local approval. Operands always use
// the invoking shell's perspective; only the remote helper converts to wire
// identities. Native SSH configuration/authentication remains effective.
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func localForwardArgs(args []string) ([]string, error) {
	if len(args) != 3 && len(args) != 4 {
		return nil, usage("use forward HOST CURRENT_PORT from|to PEER_PORT")
	}
	host := args[0]
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return nil, usage("invalid SSH destination")
	}
	m, e := parseForward(args[1:])
	if e != nil {
		return nil, e
	}
	// parseForward uses a server-shell perspective; flip only the machine identity.
	flag := "-R"
	if m.Reverse {
		flag = "-L"
	}
	spec := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", m.Port, m.RemotePort)
	return []string{"-N", "-T", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "SessionType=none", "-o", "RemoteCommand=none", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", flag, spec, "--", host}, nil
}
func runForward(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, e := parseForward(args); e != nil {
			return 2, e
		}
		return 0, serverCommand(append([]string{"forward"}, args...))
	}
	if len(args) == 1 && args[0] == "--list" {
		return 0, listLocalResources("", "forward")
	}
	if len(args) == 2 && args[0] == "--stop" {
		return 0, stopResource("forward", args[1])
	}
	var name string
	foreground := false
	var operands []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i == len(args) {
				return 2, usage("--name requires NAME")
			}
			name = args[i]
			if e := validSession(name); e != nil {
				return 2, e
			}
		case "--foreground":
			foreground = true
		default:
			operands = append(operands, args[i])
		}
	}
	args = operands
	if _, e := localForwardArgs(args); e != nil {
		return 2, e
	}
	if !foreground {
		m, _ := parseForward(args[1:])
		return startForwardSession(forwardConfig{Host: args[0], Name: name, Port: m.Port, Target: m.RemotePort, Reverse: !m.Reverse})
	}
	return runForwardForeground(args)
}
func runForwardForeground(args []string) (int, error) {
	sshArgs, e := localForwardArgs(args)
	if e != nil {
		return 2, e
	}
	exe := os.Getenv("SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Do not claim listener readiness before OpenSSH has established it.
	m, _ := parseForward(args[1:])
	fmt.Fprintf(os.Stderr, "Starting %s (%s); Ctrl-C stops it.\n", describeForward(m.Port, m.RemoteHost, m.RemotePort, !m.Reverse), cleanText(args[0]))
	if e = cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 1, e
	}
	return 0, nil
}
