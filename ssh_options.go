package main

import (
	"os"
	"strings"
)

// OpenSSH uses the first scalar value. Keep transport ownership settings ahead
// of user options so resource shutdown always closes its own SSH connection.
func ownedSSHArgs(sessionType string) []string {
	return []string{"-T", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "SessionType=" + sessionType, "-o", "RemoteCommand=none", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
}

func sftpSSHArgs(host string, ssh []string) []string {
	args := append(ownedSSHArgs("subsystem"), "-o", "ClearAllForwardings=yes")
	args = append(args, ssh...)
	return append(args, "-s", "--", host, "sftp")
}

// validSSHDestination applies to SSH aliases as well as user@host addresses.
func validSSHDestination(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.IndexFunc(host, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
		return usage("invalid SSH destination")
	}
	return nil
}

func sshExecutable() string {
	if exe := os.Getenv("SSHORE_SSH"); exe != "" {
		return exe
	}
	return "ssh"
}
