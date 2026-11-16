package main

// Control metadata and terminal bytes use bounded frames. Approved file
// streams switch to SFTP after their metadata handshake.

import (
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
	Tree            []shellTreeNode     `json:"shell_tree,omitempty"`
	Temporary       bool                `json:"temporary,omitempty"`
	Inventory       *inventory          `json:"inventory,omitempty"`
	LocalRequest    *localDaemonRequest `json:"local_request,omitempty"`
	Instance        string              `json:"instance,omitempty"`
	Token           string              `json:"token,omitempty"`
	PeerTask        *peerRecord         `json:"peer_task,omitempty"`
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
	NoProfile       bool                `json:"no_profile,omitempty"`
	NoLogin         bool                `json:"no_login,omitempty"`
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

func (m *message) FrameType() string                 { return m.Type }
func (m *message) SetFrame(kind string, data []byte) { m.Type, m.Data = kind, data }
func writeMessage(w io.Writer, m message) error {
	return transport.WriteMessage(w, m.Type, m.Data, m)
}
func readMessage(r io.Reader) (message, error) {
	var m message
	err := transport.ReadMessage(r, &m)
	return m, err
}

func fileResponseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}
