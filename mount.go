package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/files"
)

func runMount(args []string) (int, error) {

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
		return startFTPBackground(args, o)
	}
	reservation, err := reserveResourceName("mount", o.name)
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
	c := files.MountConfig{Host: o.host, Remote: o.remote, Mountpoint: o.mountpoint, SSHExecutable: sshExecutable(), Version: version, SSHArgs: mountSSHArgs(o), ReadOnly: o.readOnly, DirectIO: o.directIO, NoWatch: o.noWatch, Stats: o.stats, StartupTimeout: o.startupTimeout, IOTimeout: o.ioTimeout}
	c.Prepare = func(ctx context.Context, wire io.ReadWriter) error {
		ui := setupClientUI{}
		if s != nil {
			ui = s.setupUI(ctx)
		}
		return negotiateSetupUsing(ctx, wire, wire, &options{command: "mount-agent", host: o.host}, ui)
	}
	var lifecycle *files.MountLifecycle
	if s != nil {
		lifecycle = &files.MountLifecycle{Abandoned: s.abandoned, Environment: s.sshEnvironment(), OperationCompleted: s.operationCompleted, Ready: s.ready, Stop: s.stop, Sync: func(ctx context.Context, c *sftp.Client) func() { return startMountTaskSync(ctx, c, s.peerTask()) }}
	}
	return files.RunMount(c, lifecycle)
}

func mountSSHArgs(o ftpOptions) []string {
	if o.noWatch {
		return sftpSSHArgs(o.host, o.ssh)
	}
	command := remoteCommand(options{command: "mount-agent", server: o.server})
	args := append(ownedSSHArgs("default"), "-o", "ClearAllForwardings=yes")
	args = append(args, o.ssh...)
	return append(args, "--", o.host, "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; "+command)
}
