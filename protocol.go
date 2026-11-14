package main

// Control metadata and terminal bytes use bounded frames. Approved file
// streams switch to SFTP after their metadata handshake.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const requestTimeout = 10 * time.Second
const chunkSize = 64 * 1024
const maxFrame = chunkSize + 4096
const (
	control byte = iota
	input
	output
)

// Control metadata uses JSON; terminal bytes remain binary.
type message struct {
	PeerTask        *remoteTask    `json:"peer_task,omitempty"`
	ApprovalID      string         `json:"approval_id,omitempty"`
	Directory       string         `json:"directory,omitempty"`
	AgeSeconds      *int64         `json:"age_seconds,omitempty"`
	Names           []string       `json:"names,omitempty"`
	SetupProcesses  []setupProcess `json:"setup_processes,omitempty"`
	SoftwareVersion string         `json:"software_version,omitempty"`
	Operations      []operation    `json:"operations,omitempty"`
	Action          string         `json:"action,omitempty"`
	Type            string         `json:"type"`
	Cols            uint16         `json:"cols,omitempty"`
	Rows            uint16         `json:"rows,omitempty"`
	Term            string         `json:"term,omitempty"`
	PID             int            `json:"pid,omitempty"`
	Foreground      string         `json:"foreground,omitempty"`
	Attached        bool           `json:"attached,omitempty"`
	Name            string         `json:"name,omitempty"`
	Size            int64          `json:"size,omitempty"`
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
	if n > maxFrame || header[0] > output {
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

func fileResponseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}
