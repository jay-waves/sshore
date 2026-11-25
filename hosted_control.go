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

func resolveSession(name string, expected ...string) (string, error) {
	task, err := resolveHostedShell(name, firstExpectedInstance(expected))
	return task.Instance, err
}

func renameSession(old, name string, expected ...string) error {
	if err := validAlias(name); err != nil {
		return err
	}
	task, err := resolveHostedShell(old, firstExpectedInstance(expected))
	if err != nil {
		return err
	}
	if task.Instance == "" {
		return fmt.Errorf("shell %s does not exist", old)
	}

	// The daemon rechecks the selected label and UID when it changes the name.
	c, err := dialHostedTask(context.Background(), task.Instance, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeMessage(c, message{Type: "rename", Instance: task.Instance, Name: name, Action: task.Name, SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if m.Type != "renamed" {
		return fmt.Errorf("rename: %s", m.Error)
	}
	fmt.Fprintf(os.Stdout, "Renamed shell %s to %s\n", task.Name, name)
	return nil
}

func firstExpectedInstance(expected []string) string {
	if len(expected) > 0 {
		return expected[0]
	}
	return ""
}

// Cancellation is authorized and applied by the task owner. Process identity
// validation belongs to the daemon immediately before it signals its child.
func killSession(name string, expected ...string) error {
	id, err := resolveSession(name, expected...)
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
	case "rename":
		return 0, renameSession(o.session, o.newName, o.instance)
	default:
		return 2, usage("unsupported hosted shell action %s", o.command)
	}
}

func serveTaskControl(req remoteServiceRequest) error {
	if req.Mode != "kill" && req.Mode != "rename" {
		return usage("invalid task operation")
	}
	if err := validSession(req.Name); err != nil {
		return err
	}
	target, err := selectRegisteredTask(taskQuery{Ref: req.Name, UID: req.Instance})
	if err != nil {
		return err
	}
	_, err = mutateRegisteredTask(req.Mode, target, req.NewName)
	return err
}
