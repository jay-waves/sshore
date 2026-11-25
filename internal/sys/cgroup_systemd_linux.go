package sys

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// DaemonTaskCgroup obtains a cgroup subtree for the account daemon.
func DaemonTaskCgroup() (string, error) {
	group, err := delegatedCgroup()
	if err == nil {
		return group, nil
	}
	if scopeErr := delegateDaemonScope(); scopeErr != nil {
		return "", fmt.Errorf("Linux shells require a delegated cgroup v2 subtree: %w", errors.Join(err, scopeErr))
	}
	group, err = delegatedCgroup()
	if err != nil {
		return "", fmt.Errorf("systemd daemon scope has no writable cgroup v2 subtree: %w", err)
	}
	return group, nil
}

// Move only this new daemon process, before it publishes its listener.
func delegateDaemonScope() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := systemd.NewConnection(func() (*dbus.Conn, error) {
		bus, err := dbus.SessionBusPrivateNoAutoStartup(dbus.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if err = bus.Auth(nil); err == nil {
			err = bus.Hello()
		}
		if err != nil {
			bus.Close()
			return nil, err
		}
		return bus, nil
	})
	if err != nil {
		return fmt.Errorf("connect to systemd user manager: %w", err)
	}
	defer conn.Close()
	result := make(chan string, 1)
	properties := []systemd.Property{
		systemd.PropDescription("sshore account daemon"),
		systemd.PropPids(uint32(os.Getpid())),
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
	}
	name := "sshore-" + rand.Text() + ".scope"
	if _, err := conn.StartTransientUnitContext(ctx, name, "fail", properties, result); err != nil {
		return fmt.Errorf("start delegated systemd user scope: %w", err)
	}
	select {
	case status := <-result:
		if status != "done" {
			return fmt.Errorf("start delegated systemd user scope: job %s", status)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("start delegated systemd user scope: %w", ctx.Err())
	}
}
