package transport

import (
	"io"
	"strings"
	"sync"
)

// OpenSSH uses the first scalar value. Keep transport ownership settings ahead
// of user options so resource shutdown always closes its own SSH connection.
func OwnedSSHArgs(sessionType string) []string {
	return []string{"-T", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "SessionType=" + sessionType, "-o", "RemoteCommand=none", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
}

// Retain transport/authentication settings without replaying unrelated forwards.
func WithoutForwarding(args []string) []string {
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

// SFTPArgs selects the SSH subsystem without inheriting forwarding rules.
func SFTPArgs(host string, options []string) []string {
	args := append(OwnedSSHArgs("subsystem"), "-o", "ClearAllForwardings=yes")
	args = append(args, options...)
	return append(args, "-s", "--", host, "sftp")
}

// SessionArgs uses a caller-owned remote command and a separate SSH transport.
func SessionArgs(host string, options []string, command string) []string {
	args := append(OwnedSSHArgs("default"), "-o", "ClearAllForwardings=yes")
	args = append(args, options...)
	return append(args, "--", host, command)
}

type Pipe struct {
	io.Reader
	io.Writer
	Closers []io.Closer
	once    sync.Once
}

func (p *Pipe) Close() error {
	p.once.Do(func() {
		for _, c := range p.Closers {
			_ = c.Close()
		}
	})
	return nil
}
