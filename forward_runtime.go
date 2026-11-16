package main

// Independent forwards use an owned SSH connection and a transient remote Unix
// helper, never a shell session daemon. OpenSSH's forwarding failure gate runs
// before the helper command, whose fixed marker acknowledges startup. Killing a
// forward closes only this transport. Approved forwards require the existing
// remote executable to validate the source session; no installation is attempted.
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jay-waves/sshore/internal/forward"
	"github.com/jay-waves/sshore/internal/transport"
)

type forwardConfig = forward.Config

func validateForwardConfig(f forwardConfig) error {
	if f.Retries < 0 || f.Retries > 10 {
		return usage("--retries must be between 0 and 10")
	}
	if e := validSession(f.Name); f.Name != "" && e != nil {
		return e
	}
	if e := validSSHDestination(f.Host); e != nil {
		return e
	}
	if f.ApprovalID != "" || f.Session != "" || f.ResumePID != 0 {
		if !validApprovalID(f.ApprovalID) || validSession(f.Session) != nil || f.ResumePID <= 0 || f.Retries != 0 {
			return fmt.Errorf("invalid approved forward identity")
		}
	}
	if f.Port < 1 || f.Port > 65535 || f.Target < 1 || f.Target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
}
func startForwardSession(f forwardConfig) (int, error) {
	if e := validateForwardConfig(f); e != nil {
		return 2, e
	}
	return startResourceBackground(resourceConfig{Forward: &f}, "forward", f.Name)
}

const forwardReadyMarker = forward.ReadyMarker

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForward(f forwardConfig, s *resourceService) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stopped := make(chan resourceStopRequest, 1)
	go func() {
		select {
		case request := <-s.stop:
			stopped <- request
			cancel()
		case <-s.abandoned:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		select {
		case request := <-stopped:
			request <- nil
		default:
		}
	}()
	return forward.Run(ctx, forward.Runtime{
		Retries:     f.Retries,
		Description: describeForward(f.Port, f.Target, f.Reverse),
		Log:         s.logWriter(), HeartbeatInterval: remoteTaskRefresh,
		Command: func(ctx context.Context, initial bool) *exec.Cmd { return forwardCommand(ctx, f, s, initial) },
		Ready: func() error {
			s.record.Detail = describeForward(f.Port, f.Target, f.Reverse)
			return s.ready("", "")
		},
		Status:    s.setTransport,
		Heartbeat: func() any { return s.peerTask() },
	})
}

func forwardCommand(ctx context.Context, f forwardConfig, s *resourceService, initial bool) *exec.Cmd {
	exe := localEnvironmentValue(s.baseEnvironment(), "SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	args := forwardSSHArgs(f, true)
	if !initial {
		args = append([]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15", "-o", "ConnectionAttempts=1"}, args...)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	transport.IsolateCommand(cmd, true)
	if s.committed != nil {
		detachResourceWorker(cmd)
	}
	if initial {
		cmd.Env = s.sshEnvironment()
	} else {
		cmd.Env = s.baseEnvironment()
	}
	cmd.Dir = s.directory
	return cmd
}

// Retain transport/authentication settings without replaying unrelated forwards.
func forwardSSHOptions(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-L" || args[i] == "-R" || args[i] == "-D" {
			i++
			continue
		}
		if args[i] == "-o" && i+1 < len(args) {
			fields := strings.FieldsFunc(args[i+1], func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
			key := ""
			if len(fields) > 0 {
				key = strings.ToLower(fields[0])
			}
			if key == "localforward" || key == "remoteforward" || key == "dynamicforward" {
				i++
				continue
			}
		}
		out = append(out, args[i])
	}
	return out
}

func forwardWatchCommand(server string) string {
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; printf '" + forwardReadyMarker + "\\n'; server=" + shellQuote(server) + `; if command -v "$server" >/dev/null 2>&1; then server=$(command -v "$server"); `
	if server == "sshore" {
		command += `else server="$HOME/.local/bin/sshore"; `
	} else {
		command += `else :; `
	}
	command += `fi; if [ "$("$server" --version 2>/dev/null)" = ` + shellQuote("sshore "+version) + ` ]; then "$server" task-watch ` + shellQuote(version) + ` || exec cat >/dev/null; else exec cat >/dev/null; fi`
	return command
}

func approvedForwardCommand(f forwardConfig) string {
	server := f.Server
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; server=" + shellQuote(server) + "; if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); "
	if server == "sshore" {
		command += "else server=\"$HOME/.local/bin/sshore\"; "
	} else {
		command += "else :; "
	}
	command += "fi; exec \"$server\" forward-watch " + shellQuote(version) + " " + shellQuote(f.Session) + fmt.Sprintf(" %d ", f.ResumePID) + shellQuote(f.ApprovalID)
	return command
}
