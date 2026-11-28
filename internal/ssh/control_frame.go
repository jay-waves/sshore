package ssh

import (
	"bufio"
	"fmt"
	"io"
)

// ReadControlFrame reads a newline-delimited frame. A buffered reader retains
// any binary bytes after the frame, and the returned frame may alias its buffer.
func ReadControlFrame(r io.Reader, limit int) ([]byte, error) {
	var body []byte
	if buffered, ok := r.(interface{ ReadSlice(byte) ([]byte, error) }); ok {
		for {
			part, err := buffered.ReadSlice('\n')
			if err == nil {
				part = part[:len(part)-1]
			}
			if len(part) > limit-len(body) {
				return nil, controlFrameLimit(limit)
			}
			switch err {
			case nil:
				if body == nil {
					return part, nil
				}
				return append(body, part...), nil
			case bufio.ErrBufferFull:
				body = append(body, part...)
			default:
				if err == io.EOF && len(body)+len(part) != 0 {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
		}
	}
	// Raw streams can switch protocols after this frame. Read exactly to LF.
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			if err == io.EOF && len(body) != 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b[0] == '\n' {
			return body, nil
		}
		if len(body) == limit {
			return nil, controlFrameLimit(limit)
		}
		body = append(body, b[0])
	}
}

// WriteControlFrame takes ownership of the encoded body for the duration of
// the write, including one byte of spare capacity when available.
func WriteControlFrame(w io.Writer, body []byte, limit int) error {
	if len(body) > limit {
		return controlFrameLimit(limit)
	}
	return WriteFull(w, append(body, '\n'))
}

func controlFrameLimit(limit int) error { return fmt.Errorf("control frame exceeds %d bytes", limit) }

func WriteFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
