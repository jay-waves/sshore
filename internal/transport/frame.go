package transport

import (
	"encoding/binary"
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
