package sshtest

import (
	"context"
	"errors"
	"io"
	"os/exec"

	cryptossh "golang.org/x/crypto/ssh"
)

// Shell runs the client's remote command in a shell rooted in the test directory.
func Shell(ctx context.Context, command string, stream cryptossh.Channel, dir string, env []string) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), env...), "SSHORE_CONFIRM_ADDR=", "SSHORE_CONFIRM_TOKEN=")
	runProcess(stream, cmd)
}

func runProcess(stream cryptossh.Channel, cmd *exec.Cmd) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		Exit(stream, 255)
		return
	}
	cmd.Stdout, cmd.Stderr = stream, stream.Stderr()
	if err := cmd.Start(); err != nil {
		stdin.Close()
		Exit(stream, 255)
		return
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		defer stdin.Close()
		_, _ = io.Copy(stdin, stream)
	}()
	err = cmd.Wait()
	status := uint32(0)
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() >= 0 {
		status = uint32(exit.ExitCode())
	} else if err != nil {
		status = 255
	}
	Exit(stream, status)
	stream.Close()
	<-copied
}
