package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/tmux"
	"github.com/jay-waves/sshore/internal/wire"
	"github.com/xtaci/smux"
)

// Control metadata and terminal bytes use bounded frames. Approved file
// streams switch to SFTP after their metadata handshake.

const requestTimeout = 10 * time.Second
const chunkSize = 64 * 1024
const maxFrame = wire.MaxFrame
const (
	control byte = iota
	input
	output
)

type sessionTarget struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	Hosted   bool   `json:"hosted,omitempty"`
}

// Control metadata uses JSON; terminal bytes remain binary.
type message struct {
	OwnerProcess    *spawnProcessView   `json:"owner_process,omitempty"`
	Reason          string              `json:"reason,omitempty"`
	Handoff         *clientHandoff      `json:"handoff,omitempty"`
	SourceHost      string              `json:"source_host,omitempty"`
	SourceMachineID string              `json:"source_machine_id,omitempty"`
	Tree            []shellTreeNode     `json:"shell_tree,omitempty"`
	Temporary       bool                `json:"temporary,omitempty"`
	Inventory       *inventory          `json:"inventory,omitempty"`
	LocalRequest    *localDaemonRequest `json:"local_request,omitempty"`
	Instance        string              `json:"instance,omitempty"`
	Token           string              `json:"token,omitempty"`
	PeerTask        *peerRecord         `json:"peer_task,omitempty"`
	ApprovalID      string              `json:"approval_id,omitempty"`
	Directory       string              `json:"directory,omitempty"`
	HomeDirectory   string              `json:"home_directory,omitempty"`
	AgeSeconds      *int64              `json:"age_seconds,omitempty"`
	Targets         []sessionTarget     `json:"targets,omitempty"`
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

func validateSessionTargets(targets []sessionTarget) error {
	if len(targets) > 128 {
		return fmt.Errorf("too many session targets")
	}
	for i, target := range targets {
		if validSession(target.Name) != nil || !validTaskUID(target.Instance) ||
			slices.ContainsFunc(targets[:i], func(other sessionTarget) bool { return other.Name == target.Name }) {
			return fmt.Errorf("invalid session target")
		}
	}
	return nil
}

func sessionTargetNames(targets []sessionTarget) ([]string, error) {
	if err := validateSessionTargets(targets); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return names, nil
}

// Find the closest name in the requested direction and wrap at the end.
func adjacentSessionTarget(current string, targets []sessionTarget, previous bool) sessionTarget {
	before := func(a, b string) bool {
		if previous {
			return a > b
		}
		return a < b
	}
	var nearest, wrap sessionTarget
	for _, target := range targets {
		if target.Name == current {
			continue
		}
		if wrap.Name == "" || before(target.Name, wrap.Name) {
			wrap = target
		}
		if before(current, target.Name) && (nearest.Name == "" || before(target.Name, nearest.Name)) {
			nearest = target
		}
	}
	if nearest.Name == "" {
		return wrap
	}
	return nearest
}

func (m *message) FrameType() string                 { return m.Type }
func (m *message) SetFrame(kind string, data []byte) { m.Type, m.Data = kind, data }
func writeMessage(w io.Writer, m message) error {
	return wire.WriteMessage(w, m.Type, m.Data, m)
}
func readMessage(r io.Reader) (message, error) {
	var m message
	err := wire.ReadMessage(r, &m)
	return m, err
}

func fileResponseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}

// Logical control, terminal and file streams are application-level
// smux v2 streams over one SSH connection, not native OpenSSH channels. The
// handshake requires the exact full sshore release, including PATCH; there is no
// separate sshore protocol compatibility counter. Running daemons cannot upgrade
// in place. Per-stream windows and the shared receive budget are owned by
// wire.MuxConfig. These isolate stalled streams without
// guaranteeing real-time terminal priority: TCP head-of-line blocking still applies.
// Keepalives detect silent transport loss (10s interval, 60s timeout); detection
// releases the attachment while preserving the shell.

func clientMux(c io.ReadWriteCloser) (*smux.Session, error) {
	if err := writeMessage(c, message{Type: "mux", SoftwareVersion: version}); err != nil {
		return nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, fmt.Errorf("SSH shell handshake failed: %w", err)
	}
	if err := checkPeerVersion(m.SoftwareVersion, "remote shell daemon"); err != nil {
		return nil, err
	}
	if m.Type != "mux_ready" {
		return nil, fmt.Errorf("incompatible sshore server: %s %s", m.Type, m.Error)
	}
	return smux.Client(c, wire.MuxConfig())
}
func streamMessage(s *smux.Stream, m message) error {
	_ = s.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeMessage(s, m)
	_ = s.SetWriteDeadline(time.Time{})
	return err
}

// Check the running process, not just the executable currently on disk.
func checkPeerVersion(peer, role string) error {
	if sameRelease(version, peer) {
		return nil
	}
	if peer == "" {
		peer = "unknown (older build)"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s. Managed processes must use the same full release", version, role, peer)
}

// Accept both streams before handing ownership to a shell. Every partial
// handshake failure closes the transport here; callers own successful results.
func acceptMux(c io.ReadWriteCloser) (mx *smux.Session, control, terminal *smux.Stream, request message, err error) {
	mx, err = smux.Server(c, wire.MuxConfig())
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			mx.Close()
		}
	}()
	_ = mx.SetDeadline(time.Now().Add(5 * time.Second))
	control, err = mx.AcceptStream()
	if err != nil {
		return
	}
	_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
	request, err = readMessage(control)
	if err != nil {
		return
	}
	_ = control.SetReadDeadline(time.Time{})
	terminal, err = mx.OpenStream()
	if err != nil {
		return
	}
	err = streamMessage(terminal, message{Type: "terminal"})
	_ = mx.SetDeadline(time.Time{})
	return
}

func validateAttach(m message) error {
	if m.Type != "attach" {
		return fmt.Errorf("expected attach request")
	}
	if m.Action != "" && m.Action != "existing" {
		return fmt.Errorf("invalid attach action")
	}
	if (m.NoLogin || m.NoProfile) && m.Action != "" {
		return fmt.Errorf("startup options require a new shell")
	}
	if m.Term == "" || len(m.Term) > 128 || strings.ContainsAny(m.Term, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid TERM")
	}
	return nil
}

type attachment struct {
	*tmux.Attachment
	input            shellInput    // Lease installed and released by the shell host.
	approvalRequests chan struct{} // Hosted attachments accept approval requests.
}

func newAttachment(mx *smux.Session, control, terminal *smux.Stream) *attachment {
	return &attachment{Attachment: tmux.NewAttachment(mx, control, terminal)}
}
func (p *attachment) controlMessage(m message) error { return p.Control(m.Type, m.Data, m) }
func (p *attachment) output(m message) error         { return p.Output(m.Type, m.Data, m) }
