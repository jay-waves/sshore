package main

// Independent forwards use an owned SSH connection and a transient remote Unix
// helper, never a shell session daemon. OpenSSH's forwarding failure gate runs
// before the helper command, whose fixed marker acknowledges startup. Killing a
// worker closes only this transport. Approved forwards require the existing
// remote executable to validate the source session; no installation is attempted.
import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/transport"
)

type forwardConfig struct {
	Retries             int
	Session, ApprovalID string
	ResumePID           int
	Host, Name, Server  string
	Port, Target        int
	Reverse             bool
	SSH                 []string
}

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

const forwardReadyMarker = "sshore-forward-ready"

// Reconnect only an already-established, explicitly opted-in local forward.
// Each retry owns a new transport; application TCP connections are not resumed.
func runForwardWorker(f forwardConfig, s *resourceService) (int, error) {
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
			request.Reply <- nil
		default:
		}
	}()
	for attempt := 0; ; attempt++ {
		code, err := runForwardAttempt(ctx, f, s, attempt == 0)
		if ctx.Err() != nil {
			return 0, nil
		}
		if err == nil || attempt >= f.Retries || s.statusSnapshot().Started.IsZero() {
			return code, err
		}
		s.setTransport("reconnecting", err)
		delay := forwardRetryDelay(attempt)
		fmt.Fprintf(s.logWriter(), "[sshore] Forward disconnected; TCP connections lost. Retry %d/%d in %s.\n", attempt+1, f.Retries, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil
		case <-timer.C:
		}
	}
}
func forwardRetryDelay(attempt int) time.Duration {
	return time.Second * time.Duration(1<<min(attempt, 5))
}
func runForwardAttempt(parent context.Context, f forwardConfig, s *resourceService, initial bool) (int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
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
	var diagnostics forwardDiagnostics
	cmd.Dir = s.directory
	cmd.Stderr = io.MultiWriter(s.logWriter(), &diagnostics)
	input, e := cmd.StdinPipe()
	if e != nil {
		return 1, e
	}
	defer input.Close()
	// Let the remote watcher receive EOF and remove its lease before forcing SSH
	// down. Authentication or active forwarded sockets still have a bounded exit.
	cmd.Cancel = func() error { return input.Close() }
	cmd.WaitDelay = 2 * time.Second
	output, e := cmd.StdoutPipe()
	if e != nil {
		return 1, e
	}
	defer output.Close()
	if e = cmd.Start(); e != nil {
		return 1, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		cancel()
		input.Close()
		output.Close()
		if !exited {
			<-done
		}
	}()
	// Read only the bounded protocol marker. Unexpected login output is an error.
	marker, e := bufio.NewReader(io.LimitReader(output, int64(len(forwardReadyMarker)+2))).ReadString('\n')
	if e != nil {
		cancel()
		exitErr := <-done
		exited = true
		detail := diagnostics.String()
		if detail == "" {
			detail = fmt.Sprint(exitErr)
		}
		if strings.Contains(detail, "remote port forwarding failed") {
			detail += "; remote listening port is occupied or denied by SSH policy; choose another remote port or inspect existing forwards"
		}
		return 1, fmt.Errorf("forward startup failed (%s): %s", describeForward(f.Port, f.Target, f.Reverse), detail)
	}
	if marker != forwardReadyMarker+"\n" {
		return 1, fmt.Errorf("unexpected forward startup response: %q", cleanText(marker))
	}
	s.record.Detail = describeForward(f.Port, f.Target, f.Reverse)
	if initial {
		if e = s.ready("", ""); e != nil {
			return 1, e
		}
	} else {
		s.setTransport("connected", nil)
	}
	task := s.peerTask()
	go func() {
		ticker := time.NewTicker(remoteTaskRefresh)
		defer ticker.Stop()
		enc := json.NewEncoder(input)
		for {
			if e := enc.Encode(task); e != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	select {
	case e = <-done:
		exited = true
		if e != nil {
			return 1, e
		}
		return 1, fmt.Errorf("forward transport closed")
	case <-ctx.Done():
		return 0, nil
	}
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

// Keep only the current attempt's diagnostic tail, never earlier log entries.
type forwardDiagnostics struct {
	mu   sync.Mutex
	data []byte
}

func (d *forwardDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if len(p) >= 4096 {
		d.data = append(d.data[:0], p[len(p)-4096:]...)
	} else {
		if len(d.data)+len(p) > 4096 {
			d.data = d.data[len(d.data)+len(p)-4096:]
		}
		d.data = append(d.data, p...)
	}
	return n, nil
}
func (d *forwardDiagnostics) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cleanText(strings.TrimSpace(string(d.data)))
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
