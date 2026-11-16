package transport

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

const MaxFrame = 64*1024 + 4096

func WriteFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func WriteFrame(w io.Writer, kind byte, body []byte) error {
	if len(body) > MaxFrame {
		return fmt.Errorf("frame exceeds %d bytes", MaxFrame)
	}
	// smux may still consume a queued frame after Write returns on disconnect.
	// Own the frame so callers can safely reuse their terminal buffers.
	frame := make([]byte, 5+len(body))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	copy(frame[5:], body)
	return WriteFull(w, frame)
}
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > MaxFrame || header[0] > 2 {
		return 0, nil, fmt.Errorf("invalid frame header")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return header[0], body, err
}

// Message keeps metadata ownership with its caller while sharing frame rules.
// Binary payloads bypass JSON; control metadata must have a non-reserved type.
type Message interface {
	FrameType() string
	SetFrame(string, []byte)
}

func WriteMessage(w io.Writer, kind string, data []byte, metadata any) error {
	switch kind {
	case "input":
		return WriteFrame(w, 1, data)
	case "output":
		return WriteFrame(w, 2, data)
	default:
		body, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		return WriteFrame(w, 0, body)
	}
}

func ReadMessage(r io.Reader, metadata Message) error {
	kind, body, err := ReadFrame(r)
	if err != nil {
		return err
	}
	switch kind {
	case 1:
		metadata.SetFrame("input", body)
	case 2:
		metadata.SetFrame("output", body)
	default:
		if err := json.Unmarshal(body, metadata); err != nil {
			return err
		}
		switch metadata.FrameType() {
		case "", "input", "output", "chunk":
			return fmt.Errorf("invalid control type")
		}
	}
	return nil
}

const MaxFileStreams = 32

func MuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 * 1024
	c.MaxStreamBuffer = 1024 * 1024
	// Keep room for terminal, files and control requests. Independent forwards
	// and mounts use separate SSH connections and do not consume this budget.
	// Stalled streams must not exhaust the shared receive budget and block input.
	c.MaxReceiveBuffer = (MaxFileStreams + 12) * c.MaxStreamBuffer
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 60 * time.Second
	return c
}

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
