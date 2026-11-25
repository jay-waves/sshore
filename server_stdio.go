//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	forwardio "github.com/jay-waves/sshore/internal/forward"
)

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
