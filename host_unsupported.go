//go:build windows || (darwin && arm64)

package main

import (
	"fmt"
)

func requestOperations(command string, _ []operation) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote shell", command)
}

func handoffRemoteShell(o options) (bool, error) { return false, nil }

func resetHostedExitState(string, []string) error { return nil }
