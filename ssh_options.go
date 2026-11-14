package main

// Shared transport-option parsing keeps mount and forward authentication and
// jump-host behavior aligned. Explicit forwarding flags belong to the resource,
// not this parser. Local OpenSSH configuration remains trusted policy.
import (
	"os"
	"slices"
	"strings"
)

// -- separates product operands from normalized OpenSSH option/value pairs.
func splitSSHOptions(args []string, nativeForwarding bool) ([]string, []string, error) {
	at := slices.Index(args, "--")
	if at < 0 {
		return args, nil, nil
	}
	suffix := args[at+1:]
	if len(suffix) == 0 {
		return nil, nil, usage("expected SSH options after --")
	}
	valueFlags := "piJFloE"
	if nativeForwarding {
		valueFlags += "LRD"
	}
	var ssh []string
	for i := 0; i < len(suffix); i++ {
		a := suffix[i]
		switch a {
		case "-C", "-v", "-vv", "-vvv", "-4", "-6":
			ssh = append(ssh, a)
			continue
		}
		if len(a) < 2 || a[0] != '-' || !strings.ContainsRune(valueFlags, rune(a[1])) {
			return nil, nil, usage("unsupported SSH option after --: %s; put sshore arguments before --", a)
		}
		value := a[2:]
		if value == "" {
			i++
			if i == len(suffix) {
				return nil, nil, usage("missing value for %s", a)
			}
			value = suffix[i]
		}
		if value == "" || value == "--" {
			return nil, nil, usage("missing value for %s", a[:2])
		}
		ssh = append(ssh, a[:2], value)
	}
	return args[:at], ssh, nil
}

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

func misplacedSSHOption(a string) error {
	if len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune("piJFloELRDCv46", rune(a[1])) {
		return usage("SSH option %s must follow -- (example: sshore new HOST -- -J jump)", a)
	}
	return nil
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
