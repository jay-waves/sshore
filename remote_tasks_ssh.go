package main

// Rejection has no live session transport. Remove only its advisory remote
// record, best effort, with existing authentication and a short timeout. Never
// prompt or change known_hosts; an unreachable mirror expires independently.
import (
	"context"
	"os/exec"
	"time"
)

func removeRejectedPeerTask(r approvalRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server := r.Server
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; server=" + shellQuote(server) + "; if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); "
	if server == "sshore" {
		command += "else server=\"$HOME/.local/bin/sshore\"; "
	} else {
		command += "else :; "
	}
	command += "fi; if [ \"$(\"$server\" --version 2>/dev/null)\" = " + shellQuote("sshore "+version) + " ]; then exec \"$server\" task-remove " + shellQuote(version) + " " + shellQuote(r.ID) + "; fi"
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=2", "-o", "ConnectionAttempts=1", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ClearAllForwardings=yes", "-o", "ForkAfterAuthentication=no", "-o", "RemoteCommand=none", "-o", "SessionType=default"}
	args = append(args, forwardSSHOptions(r.SSH)...)
	args = append(args, "--", r.Host, command)
	cmd := exec.CommandContext(ctx, sshExecutable(), args...)
	cmd.Dir = r.Directory
	cmd.WaitDelay = 200 * time.Millisecond
	_ = cmd.Run()
}
