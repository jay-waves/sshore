//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func remoteDaemonPath() (string, error) {
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.control"), nil
}

// The caller holds the account setup gate, serializing daemon launch and updates.
func createRemoteShell(executable, name, label string, temporary ...bool) error {
	path, err := remoteDaemonPath()
	if err != nil {
		return err
	}
	connect := func() (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) }
	c, err := connect()
	if err != nil {
		// The last shell may have closed the listener just before its owner lock
		// is released. Never replace a live owner or race its final cleanup.
		until := time.Now().Add(3 * time.Second)
		for {
			owner, lockErr := openLock(path+".lock", true)
			if lockErr == nil {
				owner.Close()
				break
			}
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) || time.Now().After(until) {
				return fmt.Errorf("remote daemon unavailable: %w", lockErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		log, err := os.OpenFile(path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if err = privateState(path+".log", false); err != nil {
			log.Close()
			return err
		}
		cmd := exec.Command(executable, "daemon")
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = cmd.Start()
		log.Close()
		if err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
	wait:
		for {
			select {
			case err := <-done:
				return fmt.Errorf("remote daemon exited: %v; see %s.log", err, path)
			case <-deadline.C:
				return fmt.Errorf("remote daemon startup timed out; see %s.log", path)
			case <-tick.C:
				c, err = connect()
				if err == nil {
					break wait
				}
			}
		}
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	request := message{Type: "create_shell", Temporary: len(temporary) > 0 && temporary[0], SoftwareVersion: version, Name: name, Action: label, LocalRequest: &localDaemonRequest{Record: localShellRecord{Directory: cwd}, Env: os.Environ()}}
	if err = writeMessage(c, request); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "shell_created" || reply.SoftwareVersion != version {
		return fmt.Errorf("remote shell creation failed: %s", reply.Error)
	}
	return nil
}

func runSharedRemoteDaemon() error {
	path, err := remoteDaemonPath()
	if err != nil {
		return err
	}
	lock, err := openLock(path+".lock", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var mu sync.Mutex
	active := 0
	closing := false
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-done:
		}
	}()
	// An abandoned launcher must not leave an empty service behind.
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			mu.Lock()
			if active == 0 {
				closing = true
				listener.Close()
			}
			mu.Unlock()
		case <-done:
		}
	}()
	var shells sync.WaitGroup
	defer shells.Wait()
	for {
		c, err := listener.Accept()
		if err != nil {
			return nil
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := readMessage(c)
		if err != nil {
			c.Close()
			continue
		}
		fail := func(err error) {
			_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
			c.Close()
		}
		if err = checkPeerVersion(request.SoftwareVersion, "client"); err != nil {
			fail(err)
			continue
		}
		if request.Type != "create_shell" || request.LocalRequest == nil || validSession(request.Name) != nil || validSession(request.Action) != nil || !filepath.IsAbs(request.LocalRequest.Record.Directory) {
			fail(fmt.Errorf("invalid shell creation"))
			continue
		}
		mu.Lock()
		if closing {
			mu.Unlock()
			fail(fmt.Errorf("daemon is exiting; retry"))
			continue
		}
		active++
		mu.Unlock()
		shells.Add(1)
		go func() {
			defer shells.Done()
			defer c.Close()
			defer func() {
				mu.Lock()
				active--
				if active == 0 {
					closing = true
					listener.Close()
				}
				mu.Unlock()
			}()
			ready := false
			err := serveRemoteShell(request.Name, request.Action, request.LocalRequest.Env, request.LocalRequest.Record.Directory, func() error {
				err := writeMessage(c, message{Type: "shell_created", SoftwareVersion: version})
				if err == nil {
					ready = true
				}
				c.Close()
				return err
			}, request.Temporary)
			if err != nil && !ready {
				_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
			}
		}()
	}
}
