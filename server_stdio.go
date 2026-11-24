//go:build linux || windows

package main

import (
	"io"
	"os"
	"sync"
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
