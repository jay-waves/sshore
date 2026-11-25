//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/jay-waves/sshore/internal/files"
	forwardio "github.com/jay-waves/sshore/internal/forward"
)

func serveRemoteRequest(req remoteServiceRequest) error {
	switch req.Operation {
	case "shell":
		return serveRemote(req)
	case "cp-agent", "mount-agent":
		if runtime.GOOS == "windows" {
			return serveRemoteFileService(req, beginCommandActivity)
		}
		return serveRemoteFileService(req, func() (func(), error) {
			return leaseActivity(leaseRequest{Kind: "heartbeat"})
		})
	case "forward-agent":
		return serveForwardAgent(req)
	case "daemon-admin":
		if runtime.GOOS == "linux" {
			// Maintenance keeps its own snapshot and shutdown verification.
			return serveDaemonAdmin(os.Stdin, os.Stdout)
		}
	}
	if runtime.GOOS == "linux" || req.Operation == "task-control" {
		if err := checkPeerVersion(req.Release, "client"); err != nil {
			return err
		}
	}
	if req.Operation == "task-control" {
		return serveTaskControl(req)
	}
	return usage("unknown remote service operation")
}

type stdioPipe struct {
	io.Reader
	io.Writer
	closers []io.Closer
	once    sync.Once
}

func (p *stdioPipe) Close() error {
	p.once.Do(func() {
		for _, c := range p.closers {
			_ = c.Close()
		}
	})
	return nil
}

func agentStdio() *stdioPipe {
	return &stdioPipe{Reader: os.Stdin, Writer: os.Stdout, closers: []io.Closer{os.Stdin, os.Stdout}}
}

func serveRemoteFileService(req remoteServiceRequest, beginActivity func() (func(), error)) error {
	stop, err := prepareRemoteFileService(req.Release, beginActivity)
	if err != nil {
		_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		return err
	}
	defer stop()
	home, _ := os.UserHomeDir()
	if filepath.IsAbs(home) && filepath.VolumeName(home) != "" {
		home = "/" + filepath.ToSlash(home)
	}
	if err := writeMessage(os.Stdout, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", HomeDirectory: home}); err != nil {
		return err
	}
	if req.Operation == "cp-agent" {
		return files.ServeCopy(agentStdio())
	}
	account, _ := localMachineIdentity()
	return files.ServeAgentWithPeer(agentStdio(), version, account, beginIncomingMountTask)
}

func prepareRemoteFileService(peerRelease string, beginActivity func() (func(), error)) (func(), error) {
	if err := checkPeerVersion(peerRelease, "client"); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	lock, err := setupLock()
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := requireInstalledRelease(executable); err != nil {
		return nil, err
	}
	// Register the lease before reset or bootstrap can acquire the setup lock.
	return beginActivity()
}

func serveForwardAgent(req remoteServiceRequest) error {
	if err := checkPeerVersion(req.Release, "client"); err != nil {
		return err
	}
	if req.Forward == nil || req.Forward.Kind != "forward" {
		return fmt.Errorf("invalid forward request")
	}
	op := *req.Forward
	if err := (forwardConfig{Port: op.Port, Target: op.TargetPort}).Validate(); err != nil {
		return err
	}
	serveRoutedForward(agentStdio(), op)
	return nil
}

func serveRoutedForward(c io.ReadWriteCloser, op operation) {
	listenPort, targetPort := 0, op.TargetPort
	if op.Reverse {
		listenPort, targetPort = op.Port, 0
	}
	published := false
	s, err := forwardio.Start(context.Background(), listenPort, targetPort, false, func() (io.ReadWriteCloser, error) {
		published = true
		if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
			return nil, err
		}
		return c, nil
	}, func(input io.Reader) error { return watchPeerRecords(input, "forward", nil) })
	if err == nil {
		_ = s.Wait()
	} else if !published {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
}
