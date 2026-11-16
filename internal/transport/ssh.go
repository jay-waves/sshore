package transport

import (
	"io"
	"os/exec"
	"strings"
)

func connectionArgs() []string {
	return []string{"-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no"}
}

// ShellArgs preserves user forwarding settings for an interactive shell connection.
// Terminal allocation belongs to sshore rather than the SSH session.
func ShellArgs(host string, options []string, command string) []string {
	args := append(connectionArgs(), options...)
	return append(args, "-T", "-o", "RequestTTY=no", "--", host, command)
}

// OpenSSH uses the first scalar value. Keep transport ownership settings ahead
// of user options so resource shutdown always closes its own SSH connection.
func OwnedSSHArgs(sessionType string) []string {
	args := append([]string{"-T"}, connectionArgs()...)
	return append(args, "-o", "ForkAfterAuthentication=no", "-o", "SessionType="+sessionType, "-o", "RemoteCommand=none", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3")
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

// SessionArgs uses a caller-owned remote command and a separate SSH transport.
func SessionArgs(host string, options []string, command string) []string {
	args := append(OwnedSSHArgs("default"), "-o", "ClearAllForwardings=yes")
	args = append(args, options...)
	return append(args, "--", host, command)
}

// CommandPipes exposes the subprocess streams without owning its lifetime.
// Callers must finish reading before Wait and close the streams to abort blocked IO.
type CommandPipes struct {
	Input  io.WriteCloser
	Output io.ReadCloser
}

func (p *CommandPipes) Read(b []byte) (int, error)  { return p.Output.Read(b) }
func (p *CommandPipes) Write(b []byte) (int, error) { return p.Input.Write(b) }
func (p *CommandPipes) Close() error {
	_ = p.Output.Close()
	_ = p.Input.Close()
	return nil
}

// OpenPipes leaves the command unstarted so callers can configure cancellation.
func OpenPipes(cmd *exec.Cmd) (*CommandPipes, error) {
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		// StdinPipe also created the child end; Start will not clean it up here.
		if child, ok := cmd.Stdin.(io.Closer); ok {
			_ = child.Close()
		}
		return nil, err
	}
	return &CommandPipes{Input: input, Output: output}, nil
}

func StartPipes(cmd *exec.Cmd) (*CommandPipes, error) {
	pipes, err := OpenPipes(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = pipes.Close()
		return nil, err
	}
	return pipes, nil
}
