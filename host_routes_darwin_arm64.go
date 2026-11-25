package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
)

func requestOperations([]operation) error {
	return fmt.Errorf("run this command inside an sshore shell on Linux or Windows")
}

func registerPlatformDaemonRoutes(*http.ServeMux, *daemonTasks) {}

func retentionHostedShell(message) error {
	return fmt.Errorf("run this command inside an sshore shell")
}

func openHostedTransport(context.Context, options) (net.Conn, error) {
	return nil, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}

func runHostedCommand(options) (int, error) {
	return 1, fmt.Errorf("%s does not host Unix shells", runtime.GOOS)
}
