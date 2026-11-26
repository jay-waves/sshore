//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// Public names resolve through the account daemon's task table. Control and
// data connections select the immutable ID at the authenticated TCP endpoint.

func resolveSession(name, expected string) (string, error) {
	task, err := resolveHostedShell(name, expected)
	return task.Instance, err
}

// Cancellation is authorized and applied by the task owner. Process identity
// validation belongs to the daemon immediately before it signals its child.
func killSession(name, expected string) error {
	id, err := resolveSession(name, expected)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("shell %s does not exist or its instance changed", name)
	}
	c, err := dialHostedTask(context.Background(), id, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "kill", Instance: id, SoftwareVersion: version}); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "killed" {
		return fmt.Errorf("kill shell: %s", reply.Error)
	}
	fmt.Fprintf(os.Stdout, "Ended shell %s\n", name)
	return nil
}

func openHostedTransport(ctx context.Context, o options) (net.Conn, error) {
	id, err := resolveSession(o.session, o.instance)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("shell instance changed; refresh with sshore list")
	}
	return dialHostedTask(ctx, id, time.Second)
}

func runHostedCommand(o options) (int, error) {
	switch o.command {
	case "attach":
		return runClient(o)
	case "kill":
		return 0, killSession(o.session, o.instance)
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
	_, err = mutateRegisteredTask(mode, target)
	return err
}
