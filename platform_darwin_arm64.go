package main

import (
	"context"
	"errors"
	"net"
	"net/http"
)

func checkPlatform() error { return nil }
func runLocalInstaller(string) error {
	return errors.New("install the experimental macOS client manually")
}
func reportBootstrapCgroup() {}

func daemonTaskCgroup() (string, error) { return "", nil }

func serveRemoteRequest(remoteServiceRequest) error {
	return errors.New("remote services run on the Linux server")
}

func requestOperations([]operation) error {
	return errors.New("run this command inside an sshore shell on Linux or Windows")
}

func registerPlatformDaemonRoutes(*http.ServeMux, *daemonTasks) {}

func retentionHostedShell(message) error {
	return errors.New("run this command inside an sshore shell")
}

var errDarwinHostedShell = errors.New("darwin does not host Unix shells")

func openHostedTransport(context.Context, options) (net.Conn, error) {
	return nil, errDarwinHostedShell
}

func runHostedCommand(options) (int, error) {
	return 1, errDarwinHostedShell
}
