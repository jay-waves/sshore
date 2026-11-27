//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/ports"
)

func serveRemoteRequest(req remoteServiceRequest) error {
	if err := checkPeerVersion(req.Release, "client"); err != nil {
		if req.Operation != "task-control" {
			_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
		}
		return err
	}
	switch req.Operation {
	case "daemon-admin":
		return serveDaemonAdmin(os.Stdin, os.Stdout, req.Mode == "reset")
	case "task-control":
		return serveTaskControl(req.Mode, taskQuery{Ref: req.Name, UID: req.Instance})
	case "shell":
		return serveRemoteShell(req.Mode, req.Name, req.Instance, req.Directory)
	case "cp-agent":
		return serveRemoteCopy()
	case "mount-agent":
		account, _ := localMachineIdentity()
		return files.ServeAgent(agentStdio(), req.Mode, version, account, remoteFileHome(), req.PeerMetadata, beginIncomingMountTask)
	case "listeners":
		rows, err := ports.Listeners(context.Background())
		if err != nil {
			return err
		}
		return writeControlJSON(os.Stdout, rows)
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

func serveRemoteCopy() error {
	stream := agentStdio()
	if err := writeMessage(stream, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed", HomeDirectory: remoteFileHome()}); err != nil {
		return err
	}
	return files.ServeCopy(stream)
}

func remoteFileHome() string {
	home, _ := os.UserHomeDir()
	if filepath.IsAbs(home) && filepath.VolumeName(home) != "" {
		home = "/" + filepath.ToSlash(home)
	}
	return home
}

func serveForwardAgent(req *ports.Rule) error {
	if req == nil {
		return fmt.Errorf("invalid forward request")
	}
	if err := req.Validate(false); err != nil {
		return err
	}
	serveForward(agentStdio(), *req)
	return nil
}

func serveForward(c io.ReadWriteCloser, rule ports.Rule) {
	published := false
	s, err := rule.Start(context.Background(), false, func() (io.ReadWriteCloser, error) {
		published = true
		if err := writeMessage(c, message{Type: "resource_ready", SoftwareVersion: version}); err != nil {
			return nil, err
		}
		return c, nil
	}, func(input io.Reader) error { return watchPeerRecords(input, "forward", nil) })
	if err == nil {
		s.Wait()
	} else if !published {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
}
