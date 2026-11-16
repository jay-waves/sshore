package transport

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
	if err := WriteFull(w, header[:]); err != nil {
		return err
	}
	return WriteFull(w, body)
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
