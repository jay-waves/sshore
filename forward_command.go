package main

// Local requests default to independent background workers; --foreground is
// available for diagnostics. Remote shell
// requests inherit the session and require local approval. Port commands resolve
// server/local ports into fixed listener and target identities. Native SSH
// configuration/authentication remains effective.
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

func forwardSSHArgs(f forwardConfig, background bool) []string {
	sessionType := "none"
	if background {
		sessionType = "default"
	}
	args := append(ownedSSHArgs(sessionType), "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=no", "-o", "RequestTTY=no")
	flag := "-L"
	if f.Reverse {
		flag = "-R"
	}
	args = append(args, flag, fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f.Port, f.Target))
	args = append(args, forwardSSHOptions(f.SSH)...)
	args = append(args, "--", f.Host)
	if background {
		args = append(args, forwardWatchCommand(f.Server))
	}
	return args
}

func parseForwardCommand(args []string) (forwardConfig, bool, error) {
	var f forwardConfig
	var err error
	args, f.SSH, err = splitSSHOptions(args, false)
	if err != nil {
		return f, false, err
	}
	var operands []string
	foreground := false
	for i := 0; i < len(args); i++ {
		if err := misplacedSSHOption(args[i]); err != nil {
			return f, false, err
		}
		switch args[i] {
		case "--name":
			i++
			if i == len(args) {
				return f, false, usage("--name requires NAME")
			}
			f.Name = args[i]
			if e := validSession(f.Name); e != nil {
				return f, false, e
			}
		case "--retries":
			i++
			if i == len(args) {
				return f, false, usage("--retries requires a count")
			}
			n, e := strconv.Atoi(args[i])
			if e != nil || n < 0 || n > 10 {
				return f, false, usage("--retries must be between 0 and 10")
			}
			f.Retries = n
		case "--foreground":
			foreground = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, false, usage("unsupported forward option: %s", args[i])
			}
			operands = append(operands, args[i])
		}
	}
	if foreground && f.Retries != 0 {
		return f, false, usage("--retries applies only to background forwards")
	}
	f.Host, f.Port, f.Target, f.Reverse, err = parseForwardEndpoints(operands)
	return f, foreground, err
}

func runForwardForeground(f forwardConfig) (int, error) {
	sshArgs := forwardSSHArgs(f, false)

	exe := sshExecutable()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Do not claim listener readiness before OpenSSH has established it.
	fmt.Fprintf(os.Stderr, "Starting %s (%s); Ctrl-C stops it.\n", describeForward(f.Port, f.Target, f.Reverse), cleanText(f.Host))
	if e := cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 1, e
	}
	return 0, nil
}
