//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func openHostedTransport(ctx context.Context, o options) (net.Conn, error) {
	task, err := resolveHostedShell(o.session, o.instance)
	if err != nil {
		return nil, err
	}
	if task.Instance == "" {
		return nil, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return dialHostedTask(ctx, task.Instance, time.Second)
}

func runHostedCommand(o options) (int, error) {
	switch o.command {
	case "attach":
		return runClient(o)
	default:
		return 2, usage("unsupported hosted shell action %s", o.command)
	}
}

func serveTaskControl(mode string, query taskQuery) error {
	if mode != "kill" {
		return usage("invalid task operation")
	}
	if err := validSession(query.Ref); err != nil {
		return err
	}
	target, err := selectRegisteredTask(query)
	if err != nil {
		return err
	}
	_, err = killRegisteredTask(target)
	return err
}
