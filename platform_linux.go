package main

import (
	"context"

	"fmt"
	"net"
	"os"

	"strings"
	"time"
)

func stopHostedTerminal(pid int, start string) error {
	return stopOwnedProcesses([]setupProcess{{PID: pid, Start: start}})
}

// Read existing OS identity without changing system configuration.
func systemMachineID() string {
	for _, path := range []string{"/var/lib/dbus/machine-id", "/etc/machine-id"} {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
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

func localSessionInventory() inventory {
	inv, err := daemonListTasks(taskListRequest{})
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	return inv
}

func checkPlatform() error { return nil }
