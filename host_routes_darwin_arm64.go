package main

import (
	"fmt"
	"net/http"
)

func registerPlatformDaemonRoutes(*http.ServeMux, *daemonTasks) {}

func retentionHostedShell(message) error {
	return fmt.Errorf("run this command inside an sshore shell")
}
