package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

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
	c.History = mountVisitHistory
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
	c.Prepare = func(_ context.Context, wire io.ReadWriter) error {
		return negotiateSetup(wire, wire, &options{command: "mount-agent", host: o.host})
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
	return sshio.Options{Host: o.host, Args: append([]string{"-o", "ClearAllForwardings=yes"}, sshio.WithoutForwarding(o.ssh)...), Command: remoteCommand(options{command: "mount-agent", host: o.host, server: o.server}), Confirm: confirmSSHHost, Secret: promptSSHSecret}
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

// Merge this mount's observations under the account state lock on unmount.
func mountVisitHistory(account, root string, delta files.VisitHistory) files.VisitHistory {
	if !validMachineID(account) {
		return nil
	}
	dir, err := stateDir("mount-history")
	if err != nil {
		return nil
	}
	lock, err := lockStateFile(filepath.Join(dir, "state.lock"))
	if err != nil {
		return nil
	}
	defer lock.Close()
	file := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(account+"\x00"+root))))
	h := files.VisitHistory{}
	if privateStatePath(file, false) == nil {
		if info, err := os.Stat(file); err == nil && info.Size() <= files.VisitHistoryLimit*(6*4096+128) {
			if data, err := os.ReadFile(file); err == nil {
				if json.Unmarshal(data, &h) != nil || h == nil || len(h) > files.VisitHistoryLimit {
					h = files.VisitHistory{}
				}
			}
		}
	}
	h.Merge(delta, time.Now())
	if len(delta) > 0 {
		_ = writeStateJSON(file, h)
	}
	return h
}
