//go:build windows || (darwin && arm64)

package main

import (
	"context"
	"fmt"
	"net"
	"runtime"
)

func requestOperations(command string, _ []operation) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote shell", command)
}

func openHostedTransport(context.Context, options) (net.Conn, error) {
	return nil, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}

func runHostedCommand(options) (int, error) {
	return 1, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}

func handoffRemoteShell(o options) (bool, error) { return false, nil }

func resetHostedExitState(string, []string) error { return nil }
