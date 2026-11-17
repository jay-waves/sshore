package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jay-waves/sshore/internal/files"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func runMount(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, err := remoteMountRequest(args); err != nil {
			return 2, err
		}
		return 0, serverCommand(append([]string{"mount"}, args...))
	}

	o, err := parseFTPOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"mount"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if err = files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	if !o.foreground {
		return startResourceBackground(resourceConfig{Args: args}, "mount")
	}
	reservation, err := chooseTaskIdentity("mount", "")
	if err != nil {
		return 1, err
	}

	o.name = reservation.name
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	return runFTPMount(o, nil)
}

func runFTPMount(o ftpOptions, s *resourceService) (int, error) {
	c := files.MountConfig{Approval: o.approval, Host: o.host, Remote: o.remote, Mountpoint: o.mountpoint, Version: version, ReadOnly: o.readOnly, DirectIO: o.directIO, Stats: o.stats, StartupTimeout: o.startupTimeout, IOTimeout: o.ioTimeout}
	c.Connection = mountSSHOptions(o)
	if s != nil {
		c.Connection.Environment = s.sshEnvironment()
		c.Connection.Directory = s.directory
		c.Connection.Confirm = brokerSSHConfirmation(c.Connection.Environment)
		c.Connection.Secret = brokerSSHSecret(c.Connection.Environment)
	}
	if s != nil {
		c.PeerMetadata, _ = json.Marshal(s.peerTask())
	}
	c.Prepare = func(ctx context.Context, wire io.ReadWriter) error {
		ui := setupClientUI{}
		if s != nil {
			ui = s.setupUI(ctx)
		}
		return negotiateSetupUsing(ctx, wire, wire, &options{command: "mount-agent", host: o.host}, ui)
	}
	if s != nil && s.config.MountConnection != nil {
		c.Prepare = nil
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			return openRoutedResource(ctx, *s.config.MountConnection, message{Type: "route_mount", Name: o.approval.Session, PID: o.approval.PID, ApprovalID: o.approval.ID})
		}
	}
	var lifecycle *files.MountLifecycle
	if s != nil {
		lifecycle = &files.MountLifecycle{Abandoned: s.abandoned, Environment: s.sshEnvironment(), OperationCompleted: s.operationCompleted, Ready: s.ready, Stop: s.stop}
	}
	return files.RunMount(c, lifecycle)
}

func mountSSHOptions(o ftpOptions) sshio.Options {
	return sshio.Options{Host: o.host, Args: append([]string{"-o", "ClearAllForwardings=yes"}, sshio.WithoutForwarding(o.ssh)...), Command: remoteCommand(options{command: "mount-agent", server: o.server}), Confirm: confirmSSHHost, Secret: promptSSHSecret}
}

func readMountApprovalPath(input promptReader, hint string) (string, error) {
	label := hint
	if label == "" {
		label = "automatic"
	}
	for {
		line, err := input.Read(fmt.Sprintf("Mountpoint [%s] (Enter starts; Ctrl-C cancels): ", cleanText(label)), true)
		if err != nil {
			return "", fmt.Errorf("approval cancelled; request remains pending: %w", err)
		}
		if line == "" {
			line = hint
		}
		if line == "" {
			return "", nil
		}
		selected, err := termui.ParsePath([]byte(line))
		if err == nil && !(runtime.GOOS == "windows" && len(selected) == 2 && selected[1] == ':') {
			selected, err = filepath.Abs(selected)
		}
		if err == nil {
			err = files.ValidateMountpoint(selected)
		}
		if err == nil {
			return selected, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", cleanText(err.Error()))
	}
}
