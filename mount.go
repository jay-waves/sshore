package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
)

const defaultMountIOTimeout = time.Minute

// The CLI and review build the same configuration the mount worker receives.
type ftpOptions struct {
	Approval                  *files.MountApproval
	Connection                *recentConnection
	IOTimeout, StartupTimeout time.Duration
	Host, Remote, Mountpoint  string
	SSH                       []string
	ReadOnly, DirectIO, Stats bool
	Name                      string
	foreground                bool
}

func validateMountConfig(o ftpOptions) error {
	if err := validSSHDestination(o.Host); err != nil {
		return err
	}
	if o.Host == "here" || o.Host == "origin" || o.Remote == "" || strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return fmt.Errorf("invalid mount endpoints")
	}
	if o.IOTimeout < 0 || o.StartupTimeout < 0 {
		return fmt.Errorf("invalid mount timeout")
	}
	approval := o.Approval
	if approval != nil && (approval.PID <= 0 || validSession(approval.Session) != nil || !validRequestID(approval.ID)) {
		return fmt.Errorf("invalid mount approval")
	}
	if connection := o.Connection; connection != nil {
		if approval == nil || connection.Session != approval.Session || connection.Instance != approval.Instance || connection.Host != o.Host {
			return fmt.Errorf("invalid mount approval connection")
		}
		return validateRecentConnection(*connection)
	}
	if len(o.SSH) != 0 {
		_, _, err := parseSSHOptions(o.SSH, false)
		return err
	}
	return nil
}

func absoluteLocalPath(path string) (string, error) {
	path, err := termui.ExpandHomePath(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func readMountApprovalPath(input reviewPromptReader, hint string) (string, error) {
	return readReviewPath(input, "Mountpoint (empty for automatic): ", hint, true, func(path string) (string, error) {
		if !(runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':') {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			path = absolute
		}
		return path, files.ValidateMountpoint(path)
	})
}

// Merge this mount's observations under the account state lock on unmount.
func mountVisitHistory(account, root string, delta files.VisitHistory) files.VisitHistory {
	const maxHistoryBytes = files.VisitHistoryLimit * (6*4096 + 128)
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
	var h files.VisitHistory
	if err := readStateJSON(file, maxHistoryBytes, &h); err != nil || h == nil || len(h) > files.VisitHistoryLimit {
		h = files.VisitHistory{}
	}
	h.Merge(delta, time.Now())
	if len(delta) > 0 {
		_ = writeBoundedStateJSON(file, h, maxHistoryBytes)
	}
	return h
}

func executeMount(o ftpOptions) (int, error) {
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if err = files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	if !o.foreground {
		return startResourceBackground(resourceConfig{Mount: &o})
	}
	reservation, err := chooseTaskIdentity("mount", "")
	if err != nil {
		return 1, err
	}

	o.Name = reservation.name
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	return runFTPMount(o, nil, nil)
}

func runFTPMount(o ftpOptions, s *resourceService, lifecycle *files.MountLifecycle) (int, error) {
	c := files.MountConfig{Approval: o.Approval, Host: o.Host, Remote: o.Remote, Mountpoint: o.Mountpoint, Version: version, ReadOnly: o.ReadOnly, DirectIO: o.DirectIO, Stats: o.Stats, StartupTimeout: o.StartupTimeout, IOTimeout: o.IOTimeout}
	c.History = mountVisitHistory
	connection := mountSSHOptions(o)
	if s != nil {
		connection.Environment = s.baseEnvironment()
		connection.Directory = s.directory
		connection.Confirm, connection.Secret = s.config.Auth.callbacks()
		c.PeerMetadata, _ = json.Marshal(s.peerTask())
	}
	c.Prepare = func(_ context.Context, wire io.ReadWriter) (string, error) {
		return negotiateSetup(wire, wire, o.Host, false)
	}
	var data, watch *sshio.Process
	defer func() {
		if data != nil {
			_ = data.Close()
			_ = data.Wait()
		}
		if watch != nil {
			_ = watch.Wait()
		}
	}()
	c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
		var err error
		data, err = sshio.Start(ctx, connection, os.Stderr)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	c.OpenNotifications = func(_ context.Context) (io.ReadWriteCloser, error) {
		var err error
		watch, err = data.OpenSibling(connection.Command, os.Stderr)
		if err != nil {
			return nil, err
		}
		return watch, nil
	}
	if o.Connection != nil && o.Connection.viaShell() {
		c.Prepare = nil
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			return openRoutedResource(ctx, *o.Connection, message{Type: "route_mount", Name: o.Approval.Session, PID: o.Approval.PID, ApprovalID: o.Approval.ID})
		}
		c.OpenNotifications = c.Open
	}
	return files.RunMount(c, lifecycle)
}

func mountSSHOptions(o ftpOptions) sshio.Options {
	args, server := o.SSH, ""
	if o.Connection != nil {
		args, server = o.Connection.SSH, o.Connection.Server
	}
	return sshio.Options{Host: o.Host, Args: sshio.WithoutForwarding(args), Command: remoteCommand(options{command: "mount-agent", host: o.Host, server: server}), Confirm: confirmSSHHost, Secret: promptSSHSecret}
}
