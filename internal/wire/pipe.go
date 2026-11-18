package wire

import (
	"io"
	"sync"
)

type Pipe struct {
	io.Reader
	io.Writer
	Closers []io.Closer
	once    sync.Once
}

func (p *Pipe) Close() error {
	p.once.Do(func() {
		for _, c := range p.Closers {
			_ = c.Close()
		}
	})
	return nil
}
