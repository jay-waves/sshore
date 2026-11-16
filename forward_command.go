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
	if background && f.ApprovalID != "" {
		args = append(args, approvedForwardCommand(f))
	} else if background {
		args = append(args, forwardWatchCommand(f.Server))
	}
	return args
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
