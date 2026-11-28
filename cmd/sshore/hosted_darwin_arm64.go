package main

import (
	"context"
	"errors"
	"net"
	"net/http"
)

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

type hostedState struct{}

func (s *shellTask) serveHostedIngress(c net.Conn, m message) error {
	return errDarwinHostedShell
}
