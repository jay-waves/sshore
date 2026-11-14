package main

// Independent forwards use an owned SSH connection and a transient remote Unix
// helper, never a shell session daemon. OpenSSH's forwarding failure gate runs
// before the helper command, whose fixed marker acknowledges startup. Killing a
// worker closes only this transport. No remote executable is installed or kept.
import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type forwardConfig struct {
	Host, Name, Server string
	Port, Target       int
	Reverse            bool
	SSH                []string
}

func validateForwardConfig(f forwardConfig) error {
	if e := validSession(f.Name); e != nil {
		return e
	}
	if _, e := parseFTPOptions([]string{f.Host}); e != nil {
		return e
	}
	if f.Port < 1 || f.Port > 65535 || f.Target < 1 || f.Target > 65535 {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
}
func defaultForwardName(f forwardConfig) string {
	direction := "to"
	if f.Reverse {
		direction = "from"
	}
	host := ftpDefaultName(f.Host)
	if len(host) > 24 {
		host = host[:24]
	}
	return fmt.Sprintf("%s-%s-%d-%d", host, direction, f.Port, f.Target)
}
func startForwardSession(f forwardConfig) (int, error) {
	if f.Name == "" {
		f.Name = defaultForwardName(f)
	}
	if e := validateForwardConfig(f); e != nil {
		return 2, e
	}
	return startResourceBackground(resourceConfig{Forward: &f}, "forward", f.Name)
}

const forwardReadyMarker = "sshore-forward-ready"

func backgroundForwardArgs(f forwardConfig) []string {
	flag := "-L"
	if f.Reverse {
		flag = "-R"
	}
	args := []string{"-T", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "SessionType=default", "-o", "RemoteCommand=none", "-o", "RequestTTY=no", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	args = append(args, forwardSSHOptions(f.SSH)...)
	return append(args, flag, "127.0.0.1:"+strconv.Itoa(f.Port)+":127.0.0.1:"+strconv.Itoa(f.Target), "--", f.Host, forwardWatchCommand(f.Server))
}
func runForwardWorker(f forwardConfig, s *resourceService) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		select {
		case <-s.abandoned:
			cancel()
		case <-ctx.Done():
		}
	}()
	exe := os.Getenv("SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	cmd := exec.CommandContext(ctx, exe, backgroundForwardArgs(f)...)
	isolateFTPCommand(cmd)
	cmd.Env = s.sshEnvironment()
	cmd.Stderr = os.Stderr
	input, e := cmd.StdinPipe()
	if e != nil {
		return 1, e
	}
	defer input.Close()
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
		return 1, fmt.Errorf("forward startup handshake: %w", e)
	}
	if marker != forwardReadyMarker+"\n" {
		return 1, fmt.Errorf("unexpected forward startup response: %q", cleanText(marker))
	}
	s.record.Detail = describeForward(f.Port, "127.0.0.1", f.Target, f.Reverse)
	if e = s.ready("", ""); e != nil {
		return 1, e
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
	case request := <-s.stop:
		cancel()
		e = <-done
		exited = true
		// Cancellation is intentional; wait for transport exit before confirming stop.
		request.reply <- nil
		return 0, nil
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
		out = append(out, args[i])
	}
	return out
}

func forwardWatchCommand(server string) string {
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; printf '" + forwardReadyMarker + "\\n'; server=" + shellQuote(server) + `; if command -v "$server" >/dev/null 2>&1; then server=$(command -v "$server"); `
	if server == "sshore" {
		command += `else server="$HOME/.local/bin/sshore"; `
	} else {
		command += `else :; `
	}
	command += `fi; if [ "$("$server" --version 2>/dev/null)" = ` + shellQuote("sshore "+version) + ` ]; then "$server" task-watch ` + shellQuote(version) + ` || exec cat >/dev/null; else exec cat >/dev/null; fi`
	return command
}
