//go:build windows || (darwin && arm64)

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
)

func retentionHostedShell(message) error {
	return fmt.Errorf("run this command inside an sshore shell")
}

func requestOperations(command string, _ []operation) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote shell", command)
}

func hostedDiagnostics([]shellView, []int, string) {}

func currentShellDirectory(_ int, fallback string) string { return fallback }

func registerPlatformDaemonRoutes(*http.ServeMux, *daemonTasks) {}

func openHostedTransport(context.Context, options) (net.Conn, error) {
	return nil, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}

func runHostedCommand(options) (int, error) {
	return 1, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}

func handoffRemoteShell(o options) (bool, error) { return false, nil }

func resetHostedExitState(string, []string) error { return nil }
