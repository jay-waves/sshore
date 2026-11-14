package main

// The custom wire protocol frames control metadata and raw terminal/file data
// within logical streams. It is not compatible with SFTP or trzsz. File chunks
// are limited to 64 KiB; framing allows metadata overhead and rejects oversized
// messages before allocating their bodies.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const chunkSize = 64 * 1024
const maxFrame = chunkSize + 4096
const (
	control byte = iota
	input
	output
	chunk
)

// Control metadata uses JSON. Terminal and file bytes never undergo text encoding.
type message struct {
	Directory       string         `json:"directory,omitempty"`
	AgeSeconds      *int64         `json:"age_seconds,omitempty"`
	Names           []string       `json:"names,omitempty"`
	SetupProcesses  []setupProcess `json:"setup_processes,omitempty"`
	SoftwareVersion string         `json:"software_version,omitempty"`
	Operations      []operation    `json:"operations,omitempty"`
	Action          string         `json:"action,omitempty"`
	Port            int            `json:"port,omitempty"`
	Reverse         bool           `json:"reverse,omitempty"`
	RemoteHost      string         `json:"remote_host,omitempty"`
	RemotePort      int            `json:"remote_port,omitempty"`
	Ports           []int          `json:"ports,omitempty"`
	Forwards        []forwardSpec  `json:"forwards,omitempty"`
	Type            string         `json:"type"`
	ID              uint64         `json:"id,omitempty"`
	Cols            uint16         `json:"cols,omitempty"`
	Rows            uint16         `json:"rows,omitempty"`
	Term            string         `json:"term,omitempty"`
	PID             int            `json:"pid,omitempty"`
	Foreground      string         `json:"foreground,omitempty"`
	Attached        bool           `json:"attached,omitempty"`
	Name            string         `json:"name,omitempty"`
	Size            int64          `json:"size,omitempty"`
	Hash            string         `json:"hash,omitempty"`
	Error           string         `json:"error,omitempty"`
	Code            int            `json:"code,omitempty"`
	Data            []byte         `json:"-"`
}

func writeFull(w io.Writer, b []byte) error {
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
func writeMessage(w io.Writer, m message) error {
	kind := control
	var body []byte
	switch m.Type {
	case "input":
		kind, body = input, m.Data
	case "output":
		kind, body = output, m.Data
	case "chunk":
		kind = chunk
		body = make([]byte, 8+len(m.Data))
		binary.BigEndian.PutUint64(body, m.ID)
		copy(body[8:], m.Data)
	default:
		var err error
		body, err = json.Marshal(m)
		if err != nil {
			return err
		}
	}
	if len(body) > maxFrame {
		return fmt.Errorf("frame exceeds %d bytes", maxFrame)
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, body)
}
func readMessage(r io.Reader) (message, error) {
	var m message
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > maxFrame || header[0] > chunk {
		return m, fmt.Errorf("invalid frame header")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return m, err
	}
	switch header[0] {
	case input:
		m.Type, m.Data = "input", body
	case output:
		m.Type, m.Data = "output", body
	case chunk:
		if len(body) < 8 {
			return m, fmt.Errorf("short file frame")
		}
		m.Type, m.ID, m.Data = "chunk", binary.BigEndian.Uint64(body), body[8:]
	default:
		if err := json.Unmarshal(body, &m); err != nil {
			return m, err
		}
		if m.Type == "" || m.Type == "input" || m.Type == "output" || m.Type == "chunk" {
			return m, fmt.Errorf("invalid control type")
		}
	}
	return m, nil
}
