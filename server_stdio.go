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
	// Maintenance keeps its own snapshot and shutdown verification.
	if req.Operation != "daemon-admin" {
		if err := checkPeerVersion(req.Release, "client"); err != nil {
			if req.Operation != "task-control" {
				_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
			}
			return err
		}
	}
	switch req.Operation {
	case "daemon-admin":
		if runtime.GOOS == "linux" {
			return serveDaemonAdmin(os.Stdin, os.Stdout)
		}
	case "task-control":
		return serveTaskControl(req.Mode, taskQuery{Ref: req.Name, UID: req.Instance})
	case "shell":
		return serveRemoteShell(req.Mode, req.Name, req.Instance, req.Directory)
	case "cp-agent":
		return serveRemoteFileService(files.ServeCopy)
	case "mount-agent":
		return withRemoteFileService(func(stream io.ReadWriteCloser) error {
			account, _ := localMachineIdentity()
			return files.ServeAgent(stream, req.Mode, version, account, remoteFileHome(), req.PeerMetadata, beginIncomingMountTask)
		})
	case "forward-agent":
		return serveForwardAgent(req.Forward)
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

func serveRemoteFileService(serve func(io.ReadWriteCloser) error) error {
	return withRemoteFileService(func(stream io.ReadWriteCloser) error {
		if err := writeMessage(stream, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", HomeDirectory: remoteFileHome()}); err != nil {
			return err
		}
		return serve(stream)
	})
}

func withRemoteFileService(serve func(io.ReadWriteCloser) error) error {
	stop, err := prepareRemoteFileService()
	if err != nil {
		_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		return err
	}
	defer stop()
	return serve(agentStdio())
}

func remoteFileHome() string {
	home, _ := os.UserHomeDir()
	if filepath.IsAbs(home) && filepath.VolumeName(home) != "" {
		home = "/" + filepath.ToSlash(home)
	}
	return home
}

func prepareRemoteFileService() (func(), error) {
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
	if runtime.GOOS == "windows" {
		return beginCommandActivity()
	}
	return leaseActivity(leaseRequest{Kind: "heartbeat"})
}

func serveForwardAgent(req *remoteForwardRequest) error {
	if req == nil || req.Kind != "forward" {
		return fmt.Errorf("invalid forward request")
	}
	if err := (forwardConfig{Port: req.Port, Target: req.TargetPort}).Validate(); err != nil {
		return err
	}
	serveForward(agentStdio(), req.Port, req.TargetPort, req.Reverse)
	return nil
}

func serveForward(c io.ReadWriteCloser, port, targetPort int, reverse bool) {
	listenPort := 0
	if reverse {
		listenPort, targetPort = port, 0
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
