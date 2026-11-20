package pty

import "os/exec"

func waitTerminalChild(cmd *exec.Cmd) error { return cmd.Wait() }
