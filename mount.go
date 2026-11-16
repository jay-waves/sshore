package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/transport"
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
	if err = files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	if !o.foreground {
		return startResourceBackground(resourceConfig{Args: args}, "mount")
	}
	reservation, err := reserveResourceName("mount", "")
	if err != nil {
		return 1, err
	}
	defer reservation.Close()
	o.name = reservation.name
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	return runFTPMount(o, nil)
}

func runFTPMount(o ftpOptions, s *resourceService) (int, error) {
	c := files.MountConfig{Approval: o.approval, Host: o.host, Remote: o.remote, Mountpoint: o.mountpoint, SSHExecutable: sshExecutable(), Version: version, SSHArgs: mountSSHArgs(o), ReadOnly: o.readOnly, DirectIO: o.directIO, NoWatch: o.noWatch, Stats: o.stats, StartupTimeout: o.startupTimeout, IOTimeout: o.ioTimeout}
	c.Prepare = func(ctx context.Context, wire io.ReadWriter) error {
		ui := setupClientUI{}
		if s != nil {
			ui = s.setupUI(ctx)
		}
		return negotiateSetupUsing(ctx, wire, wire, &options{command: "mount-agent", host: o.host}, ui)
	}
	var lifecycle *files.MountLifecycle
	if s != nil {
		lifecycle = &files.MountLifecycle{Abandoned: s.abandoned, Environment: s.sshEnvironment(), OperationCompleted: s.operationCompleted, Ready: s.ready, Stop: s.stop, Sync: func(ctx context.Context, c *sftp.Client) func() { return startMountTaskSync(ctx, c, s.peerTask) }}
	}
	return files.RunMount(c, lifecycle)
}

func mountSSHArgs(o ftpOptions) []string {
	if o.noWatch {
		return transport.SFTPArgs(o.host, o.ssh)
	}
	command := remoteCommand(options{command: "mount-agent", server: o.server})
	return transport.SessionArgs(o.host, o.ssh, "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; "+command)
}

func readMountApprovalPath(input promptReader, hint string) (string, error) {
	label := hint
	if label == "" {
		label = "automatic"
	}
	for {
		line, err := input.Read(fmt.Sprintf("Mountpoint [%s] (Tab; Enter accepts): ", cleanText(label)), true)
		if err != nil {
			return "", fmt.Errorf("approval cancelled; request remains pending: %w", err)
		}
		if line == "" {
			line = hint
		}
		if line == "" {
			return "", nil
		}
		selected, err := localPath([]byte(line))
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
