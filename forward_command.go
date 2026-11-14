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
	"strconv"
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
	f, operands, foreground, e := parseForwardCommand(args)
	if e != nil {
		return 2, e
	}
	if !foreground {
		return startForwardSession(f)
	}
	return runForwardForegroundWithSSH(operands, f.SSH)
}

func parseForwardCommand(args []string) (forwardConfig, []string, bool, error) {
	var f forwardConfig
	var err error
	args, f.SSH, err = splitSSHOptions(args, false)
	if err != nil {
		return f, nil, false, err
	}
	var operands []string
	foreground := false
	for i := 0; i < len(args); i++ {
		if err := misplacedSSHOption(args[i]); err != nil {
			return f, nil, false, err
		}
		switch args[i] {
		case "--name":
			i++
			if i == len(args) {
				return f, nil, false, usage("--name requires NAME")
			}
			f.Name = args[i]
			if e := validSession(f.Name); e != nil {
				return f, nil, false, e
			}
		case "--retries":
			i++
			if i == len(args) {
				return f, nil, false, usage("--retries requires a count")
			}
			n, e := strconv.Atoi(args[i])
			if e != nil || n < 0 || n > 10 {
				return f, nil, false, usage("--retries must be between 0 and 10")
			}
			f.Retries = n
		case "--foreground":
			foreground = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, nil, false, usage("unsupported forward option: %s", args[i])
			}
			operands = append(operands, args[i])
		}
	}
	if foreground && f.Retries != 0 {
		return f, nil, false, usage("--retries applies only to background forwards")
	}
	if _, e := localForwardArgs(operands); e != nil {
		return f, nil, false, e
	}
	m, _ := parseForward(operands[1:])
	f.Host, f.Port, f.Target, f.Reverse = operands[0], m.Port, m.RemotePort, !m.Reverse
	return f, operands, foreground, nil
}

func runForwardForeground(args []string) (int, error) {
	return runForwardForegroundWithSSH(args, nil)
}
func runForwardForegroundWithSSH(args, transport []string) (int, error) {
	sshArgs, e := localForwardArgs(args)
	if e != nil {
		return 2, e
	}
	// Insert options before the destination delimiter, preserving fixed ownership options.
	sshArgs = append(append(append([]string{}, sshArgs[:len(sshArgs)-2]...), transport...), sshArgs[len(sshArgs)-2:]...)
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
