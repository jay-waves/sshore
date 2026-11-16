package main

import (
	"os"
	"strings"
)

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
