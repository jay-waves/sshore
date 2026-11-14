package main

// Mounts use their own SSH subprocess, never a shared ControlMaster or a session
// daemon. Cancelling/losing that transport cannot terminate a persistent shell.
// Normal interruption unmounts while SSH remains alive so callbacks can finish.
// A second interrupt or grace timeout aborts transport to release blocked work.
// Forced teardown may interrupt writes; it never retries them or spools them.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"syscall"
	"time"
)

type ftpMount interface {
	Unmount() error
	Wait()
}

func ftpSSHArgs(o ftpOptions) []string {
	// OpenSSH uses the first value for scalar options. These precede user/config
	// settings to guarantee this process owns a foreground, subsystem transport.
	args := []string{"-T", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "SessionType=subsystem", "-o", "RemoteCommand=none", "-o", "ClearAllForwardings=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	args = append(args, o.ssh...)
	return append(args, "-s", "--", o.host, "sftp")
}

func runFTP(args []string) (int, error) {
	if len(args) == 1 && args[0] == "--list" {
		return 0, listFTPSessions()
	}
	if len(args) > 0 && args[0] == "--stop" {
		if len(args) != 2 {
			return 2, usage("ftp --stop requires NAME")
		}
		return 0, stopFTPSession(args[1])
	}

	o, err := parseFTPOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"ftp"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	if err = checkFTPPlatform(); err != nil {
		return 1, err
	}
	if !o.foreground {
		return startFTPBackground(args, o)
	}
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	return runFTPMount(o, nil)
}

func runFTPMount(o ftpOptions, service *resourceService) (int, error) {
	var err error
	if err = checkFTPPlatform(); err != nil {
		return 1, err
	}
	if err = validateFTPMountpoint(o.mountpoint); err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	var abandoned <-chan struct{}
	if service != nil {
		abandoned = service.abandoned
	}
	setupFinished, setupStopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(setupStopped)
		select {
		case <-abandoned:
			cancel()
		case <-interrupts:
			cancel()
		case <-setupFinished:
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-setupStopped }()
	exe := os.Getenv("SSHORE_SSH")
	if exe == "" {
		exe = "ssh"
	}
	cmd := exec.CommandContext(ctx, exe, ftpSSHArgs(o)...)
	isolateFTPCommand(cmd)
	cmd.Stderr = os.Stderr
	if service != nil {
		cmd.Env = service.sshEnvironment()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	if err = cmd.Start(); err != nil {
		return 1, err
	}
	defer func() {
		_ = stdin.Close()
		_ = stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	client, err := newFTPSFTP(stdout, stdin)
	if err != nil {
		return 1, fmt.Errorf("SFTP handshake: %w", err)
	}
	// Direct pipe closure must precede Client.Close: pkg/sftp serializes Close
	// with writes and otherwise cannot interrupt a blocked outgoing request.
	defer func() { cancel(); _ = stdout.Close(); _ = stdin.Close(); _ = client.Close() }()
	root, err := client.RealPath(o.remote)
	if err != nil {
		return 1, fmt.Errorf("remote directory: %w", err)
	}
	info, err := client.Stat(root)
	if err != nil {
		return 1, err
	}
	if !info.IsDir() {
		return 1, fmt.Errorf("remote path is not a directory")
	}
	if !o.readOnly {
		for _, ext := range []string{"posix-rename@openssh.com", "fsync@openssh.com"} {
			if _, ok := client.HasExtension(ext); !ok {
				return 1, fmt.Errorf("writable mounts require SFTP extension %s; use --read-only or update the server", ext)
			}
		}
	}
	backend := &ftpFS{client: client, root: path.Clean(root), readOnly: o.readOnly, directIO: o.directIO}
	mount, err := mountFTP(backend, o.mountpoint)
	if err != nil {
		return 1, err
	}
	close(setupFinished)
	<-setupStopped
	if service != nil {
		if err = service.ready(root, o.mountpoint); err != nil {
			cancel()
			_ = stdout.Close()
			_ = stdin.Close()
			_ = mount.Unmount()
			return 1, err
		}
	}
	if service != nil {
		stopSync := startMountTaskSync(ctx, client, service.peerTask())
		defer stopSync()
	}
	fmt.Fprintf(os.Stderr, "Mounted %s:%s at %s. Ctrl-C unmounts; close files first.\n", cleanText(o.host), cleanText(root), cleanText(o.mountpoint))
	done := make(chan struct{})
	go func() { mount.Wait(); close(done) }()
	lost := make(chan error, 1)
	go func() { lost <- client.Wait() }()
	abort := func() { cancel(); _ = stdout.Close(); _ = stdin.Close() }
	var requests <-chan resourceStopRequest
	if service != nil {
		requests = service.stop
	}
	for {
		select {
		case request := <-requests:
			stopped, stopErr := stopFTPMount(mount, interrupts, abort, 10*time.Second)
			request.reply <- stopErr
			if stopped {
				if stopErr != nil {
					return 1, stopErr
				}
				return 0, nil
			}
		case <-done:
			return 0, nil
		case <-ctx.Done():
			abort()
			_ = mount.Unmount()
			return 1, fmt.Errorf("mount connection interrupted during setup")
		case <-interrupts:
			fmt.Fprintln(os.Stderr, "Unmounting; waiting for active file operations (up to 10s). Ctrl-C again forces disconnect.")
			stopped, stopErr := stopFTPMount(mount, interrupts, abort, 10*time.Second)
			if !stopped {
				fmt.Fprintf(os.Stderr, "Unmount failed: %s. Close open files and retry Ctrl-C; mount remains connected.\n", cleanText(stopErr.Error()))
				continue
			}
			if stopErr != nil {
				return 1, stopErr
			}
			return 0, nil
		case err = <-lost:
			abort()
			_, unmountErr := stopFTPMount(mount, interrupts, abort, 5*time.Second)
			if unmountErr != nil {
				return 1, fmt.Errorf("SFTP disconnected (%v); unmount: %w", err, unmountErr)
			}
			return 1, fmt.Errorf("SFTP disconnected: %v", err)
		}
	}
}

// stopFTPMount leaves a busy mount connected after a normal unmount refusal.
// It only interrupts transport after the grace period or an explicit second
// signal. Waiting is bounded even if a platform's unmount cannot be interrupted.
func stopFTPMount(m ftpMount, interrupts <-chan os.Signal, abort func(), grace time.Duration) (bool, error) {
	result := make(chan error, 1)
	go func() { result <- m.Unmount() }()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	reason := "unmount timed out"
	select {
	case e := <-result:
		if e != nil {
			return false, e
		}
		return true, nil
	case <-interrupts:
		reason = "forced disconnect"
	case <-timer.C:
	}
	abort()
	timer.Reset(5 * time.Second)
	select {
	case e := <-result:
		if e != nil {
			return true, fmt.Errorf("%s; unmount failed: %w", reason, e)
		}
	case <-timer.C:
		return true, fmt.Errorf("%s; unmount still pending; in-flight writes may be incomplete", reason)
	}
	return true, fmt.Errorf("%s; in-flight writes may be incomplete", reason)
}
