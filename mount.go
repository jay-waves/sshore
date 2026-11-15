package main

import (
	"context"
	"errors"
	"os"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/mount"
)

func runMount(args []string) (int, error) {

	o, err := parseFTPOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"mount"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	if err = mount.CheckPlatform(); err != nil {
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
	c := mount.Config{Host: o.host, Remote: o.remote, Mountpoint: o.mountpoint, SSHExecutable: sshExecutable(), Version: version, SSHArgs: mountSSHArgs(o), ReadOnly: o.readOnly, DirectIO: o.directIO, NoWatch: o.noWatch, Stats: o.stats, StartupTimeout: o.startupTimeout, IOTimeout: o.ioTimeout}
	var lifecycle *mount.Lifecycle
	if s != nil {
		lifecycle = &mount.Lifecycle{Abandoned: s.abandoned, Environment: s.sshEnvironment(), OperationCompleted: s.operationCompleted, Ready: s.ready, Stop: s.stop, Sync: func(ctx context.Context, c *sftp.Client) func() { return startMountTaskSync(ctx, c, s.peerTask()) }}
	}
	return mount.Run(c, lifecycle)
}
