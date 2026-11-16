package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/jay-waves/sshore/internal/transport"
)

var ErrRetries = errors.New("--retries must be between 0 and 10")

var ErrPort = errors.New("port must be an integer from 1 to 65535")

type Config struct {
	Retries      int
	Host         string
	Port, Target int
	Reverse      bool
	SSH          []string
}

func ParsePort(s string) (int, error) {
	if s == "" {
		return 0, ErrPort
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrPort
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, ErrPort
	}
	return n, nil
}

// Validate checks transport rules; names, approvals and release policy belong to the application.
func (c Config) Validate() error {
	if !validRetries(c.Retries) {
		return ErrRetries
	}
	if c.Port < 1 || c.Port > 65535 || c.Target < 1 || c.Target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
}

// SSHArgs builds an owned forwarding transport. A nonempty remote command selects
// the readiness/heartbeat protocol; its authorization and version policy belong to the caller.
func SSHArgs(c Config, remoteCommand string) []string {
	sessionType := "none"
	if remoteCommand != "" {
		sessionType = "default"
	}
	args := append(transport.OwnedSSHArgs(sessionType), "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=no", "-o", "RequestTTY=no")
	flag := "-L"
	if c.Reverse {
		flag = "-R"
	}
	args = append(args, flag, fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", c.Port, c.Target))
	args = append(args, transport.WithoutForwarding(c.SSH)...)
	args = append(args, "--", c.Host)
	if remoteCommand != "" {
		args = append(args, remoteCommand)
	}
	return args
}

// Command constructs a fresh transport. Reconnect never prompts for credentials
// or trusts a new host key. The caller supplies environment and resource ownership.
func Command(ctx context.Context, executable string, c Config, remoteCommand string, retry bool) *exec.Cmd {
	args := SSHArgs(c, remoteCommand)
	if retry {
		args = append([]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15", "-o", "ConnectionAttempts=1"}, args...)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	if remoteCommand != "" {
		transport.IsolateCommand(cmd, true)
	}
	return cmd
}

// RunForeground uses the caller's streams and cancellation without background
// readiness markers, heartbeats or retries.
func RunForeground(ctx context.Context, executable string, c Config, in io.Reader, out, diagnostic io.Writer) (int, error) {
	cmd := Command(ctx, executable, c, "", false)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, diagnostic
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		return 1, err
	}
	return 0, nil
}

// ParseRetries applies the same retry policy to CLI and persisted configuration.
func ParseRetries(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || !validRetries(n) {
		return 0, ErrRetries
	}
	return n, nil
}

func validRetries(n int) bool { return n >= 0 && n <= 10 }
