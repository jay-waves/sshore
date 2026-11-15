package main

// Control metadata and terminal bytes use bounded frames. Approved file
// streams switch to SFTP after their metadata handshake.

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jay-waves/sshore/internal/transport"
)

const requestTimeout = 10 * time.Second
const chunkSize = 64 * 1024
const maxFrame = transport.MaxFrame
const (
	control byte = iota
	input
	output
)

// Control metadata uses JSON; terminal bytes remain binary.
type message struct {
	Temporary       bool                `json:"temporary,omitempty"`
	Inventory       *inventory          `json:"inventory,omitempty"`
	LocalRequest    *localDaemonRequest `json:"local_request,omitempty"`
	Instance        string              `json:"instance,omitempty"`
	Token           string              `json:"token,omitempty"`
	PeerTask        *remoteTask         `json:"peer_task,omitempty"`
	ApprovalID      string              `json:"approval_id,omitempty"`
	Directory       string              `json:"directory,omitempty"`
	AgeSeconds      *int64              `json:"age_seconds,omitempty"`
	Names           []string            `json:"names,omitempty"`
	SetupProcesses  []setupProcess      `json:"setup_processes,omitempty"`
	SoftwareVersion string              `json:"software_version,omitempty"`
	Operations      []operation         `json:"operations,omitempty"`
	Action          string              `json:"action,omitempty"`
	Type            string              `json:"type"`
	Cols            uint16              `json:"cols,omitempty"`
	Rows            uint16              `json:"rows,omitempty"`
	Term            string              `json:"term,omitempty"`
	PID             int                 `json:"pid,omitempty"`
	Foreground      string              `json:"foreground,omitempty"`
	Attached        bool                `json:"attached,omitempty"`
	Name            string              `json:"name,omitempty"`
	Size            int64               `json:"size,omitempty"`
	Error           string              `json:"error,omitempty"`
	Code            int                 `json:"code,omitempty"`
	Data            []byte              `json:"-"`
}

func writeFull(w io.Writer, b []byte) error { return transport.WriteFull(w, b) }
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
	return transport.WriteFrame(w, kind, body)
}

func readMessage(r io.Reader) (message, error) {
	var m message
	kind, body, err := transport.ReadFrame(r)
	if err != nil {
		return m, err
	}
	switch kind {
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
