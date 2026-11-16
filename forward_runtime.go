package main

// Independent forwards use an owned SSH connection and a transient remote Unix
// helper, never a shell session daemon. OpenSSH's forwarding failure gate runs
// before the helper command, whose fixed marker acknowledges startup. Killing a
// forward closes only this transport. Approved forwards require the existing
// remote executable to validate the source session; no installation is attempted.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/jay-waves/sshore/internal/forward"
)

// Application identity stays outside the forwarding engine and preserves stored records.
type forwardConfig struct {
	Retries             int
	Session, ApprovalID string
	ResumePID           int
	Host, Name, Server  string
	Port, Target        int
	Reverse             bool
	SSH                 []string
}

func (f forwardConfig) transportConfig() forward.Config {
	return forward.Config{Retries: f.Retries, Host: f.Host, Port: f.Port, Target: f.Target, Reverse: f.Reverse, SSH: f.SSH}
}

func validateForwardConfig(f forwardConfig) error {
	transportErr := f.transportConfig().Validate()
	if errors.Is(transportErr, forward.ErrRetries) {
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
	return transportErr
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
	var stopped resourceStopRequest
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case stopped = <-s.stop:
			cancel()
		case <-s.abandoned:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-watcherDone
		if stopped != nil {
			stopped <- nil
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
		Status: s.setTransport,
		Heartbeat: func() ([]byte, error) {
			data, err := json.Marshal(s.peerTask())
			return append(data, '\n'), err
		},
	})
}

func forwardCommand(ctx context.Context, f forwardConfig, s *resourceService, initial bool) *exec.Cmd {
	var environment []string
	if initial {
		environment = s.sshEnvironment()
	} else {
		environment = s.baseEnvironment()
	}
	exe := localEnvironmentValue(environment, "SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	cmd := forward.Command(ctx, exe, f.transportConfig(), forwardRemoteCommand(f), !initial)
	if s.committed != nil {
		detachResourceWorker(cmd)
	}
	cmd.Env = environment
	cmd.Dir = s.directory
	return cmd
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
