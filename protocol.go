package main

import (
	"bufio"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Control metadata uses bounded frames. Approved file streams switch to
// SFTP after their metadata handshake.

const requestTimeout = 10 * time.Second

type sessionTarget struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	Hosted   bool   `json:"hosted,omitzero"`
}

// Control metadata uses JSON; terminal bytes remain binary.
type retainedShell struct {
	Host, Instance, Name string
	PinUntil             time.Time
}

type resourceHop struct {
	Parent string
	Token  string
}

type terminalDisplay struct {
	Cols  uint16 `json:"cols,omitzero"`
	Rows  uint16 `json:"rows,omitzero"`
	Color bool   `json:"color,omitzero"`
	Term  string `json:"term,omitempty"`
}

type consoleFilePayload struct {
	Files           []directoryEntry `json:"files,omitempty"`
	ParentDirectory string           `json:"parent_directory,omitempty"`
	ParentFiles     []directoryEntry `json:"parent_files,omitempty"`
	ParentError     string           `json:"parent_error,omitempty"`
}

type hostAnnouncement struct {
	Info      hostInfo `json:"host_info,omitzero"`
	SSHServer string   `json:"ssh_server_ip,omitempty"`
}

type message struct {
	terminalDisplay  `json:",embed"`
	ConsoleFiles     *consoleFilePayload `json:",embed"`
	Host             *hostAnnouncement   `json:",embed"`
	ResourceRoute    []resourceHop       `json:"resource_route,omitempty"`
	NativeTargets    bool                `json:"native_targets,omitzero"`
	RetainedShells   []retainedShell     `json:"retained_shells,omitempty"`
	Owner            string              `json:"owner,omitempty"`
	TreeKey          string              `json:"tree_key,omitempty"`
	TTLNanos         int64               `json:"ttl_nanos,omitzero"`
	PinUntil         time.Time           `json:"pin_until,omitzero"`
	TerminalOffset   uint64              `json:"terminal_offset,omitzero"`
	OutputIncomplete bool                `json:"output_incomplete,omitzero"`
	DaemonShutdown   bool                `json:"daemon_shutdown,omitzero"`
	Reason           string              `json:"reason,omitempty"`
	Handoff          *clientHandoff      `json:"handoff,omitempty"`
	SourceHost       string              `json:"source_host,omitempty"`
	MachineID        string              `json:"machine_id,omitempty"`
	Pinned           bool                `json:"pinned,omitzero"`
	LocalRequest     *localDaemonRequest `json:"local_request,omitempty"`
	Instance         string              `json:"instance,omitempty"`
	Token            string              `json:"token,omitempty"`
	HandoffToken     string              `json:"handoff_token,omitempty"`
	ApprovalID       string              `json:"approval_id,omitempty"`
	Directory        string              `json:"directory,omitempty"`
	HomeDirectory    string              `json:"home_directory,omitempty"`
	Targets          []sessionTarget     `json:"targets,omitempty"`
	SoftwareVersion  string              `json:"software_version,omitempty"`
	Operations       []operation         `json:"operations,omitempty"`
	Action           string              `json:"action,omitempty"`
	Type             string              `json:"type"`
	Argv             []string            `json:"argv,omitempty"`
	NoProfile        bool                `json:"no_profile,omitzero"`
	NoLogin          bool                `json:"no_login,omitzero"`
	PID              int                 `json:"pid,omitzero"`
	Name             string              `json:"name,omitempty"`
	Shell            string              `json:"shell,omitempty"`
	Program          bool                `json:"program,omitzero"`
	Size             int64               `json:"size,omitzero"`
	Output           string              `json:"output,omitempty"`
	Error            string              `json:"error,omitempty"`
	Code             int                 `json:"code,omitzero"`
}

func (m *message) advertisedHost() hostInfo {
	if m.Host == nil {
		return hostInfo{}
	}
	return m.Host.Info
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

const maxControlMessage = 64*1024 + 4096

func writeMessage(w io.Writer, m message) error { return writeControlJSON(w, m) }

func writeControlJSON(w io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > maxControlMessage {
		return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	return writeFull(w, append(body, '\n'))
}

func readMessage(r io.Reader) (message, error) {
	var m message
	if err := readControlJSON(r, &m); err != nil {
		return m, err
	}
	switch m.Type {
	case "", "input", "output", "chunk":
		return m, fmt.Errorf("invalid control type")
	}
	return m, nil
}

func readControlJSON(r io.Reader, target any, opts ...json.Options) error {
	var body []byte
	if lines, ok := r.(interface{ ReadSlice(byte) ([]byte, error) }); ok {
		for {
			part, err := lines.ReadSlice('\n')
			if len(body)+len(part) > maxControlMessage+1 || err == bufio.ErrBufferFull && len(body)+len(part) > maxControlMessage {
				return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
			}
			if err == nil {
				body = append(body, part[:len(part)-1]...)
				break
			}
			if err != bufio.ErrBufferFull {
				if err == io.EOF && len(body)+len(part) != 0 {
					err = io.ErrUnexpectedEOF
				}
				return err
			}
			body = append(body, part...)
		}
	} else {
		// Unbuffered readers may hand this same stream to a raw protocol.
		var b [1]byte
		for {
			if _, err := io.ReadFull(r, b[:]); err != nil {
				if err == io.EOF && len(body) != 0 {
					err = io.ErrUnexpectedEOF
				}
				return err
			}
			if b[0] == '\n' {
				break
			}
			if len(body) == maxControlMessage {
				return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
			}
			body = append(body, b[0])
		}
	}
	return json.Unmarshal(body, target, opts...)
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

func fileResponseError(m message) error {
	if m.Error != "" {
		return fmt.Errorf("%s", m.Error)
	}
	return fmt.Errorf("transfer cancelled or unexpected response: %s", m.Type)
}

func streamMessage(s attachmentControl, m message) error {
	_ = s.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeMessage(s, m)
	_ = s.SetWriteDeadline(time.Time{})
	return err
}

// Check the running process, not just the executable currently on disk.
func checkPeerVersion(peer, role string) error {
	return checkReleaseVersions(version, peer, role)
}

func checkReleaseVersions(local, peer, role string) error {
	if local == peer {
		if _, ok := releaseParts(local); ok {
			return nil
		}
	}
	if peer == "" {
		peer = "unknown"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%q", local, role, peer)
}

func releaseParts(v string) ([3]string, bool) {
	major, rest, hasMinor := strings.Cut(v, ".")
	minor, patch, hasPatch := strings.Cut(rest, ".")
	if !hasMinor || !hasPatch {
		return [3]string{}, false
	}
	parts := [3]string{major, minor, patch}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return [3]string{}, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return [3]string{}, false
			}
		}
	}
	return parts, true
}

// Prefer a reported release mismatch to the peer's error text. Relays can
// report failures before they have learned the peer's release.
func (m message) peerError(role string) error {
	if (m.Type == "fatal" || m.Type == "error") &&
		(m.SoftwareVersion == "" || m.SoftwareVersion == version) {
		return errors.New(m.Error)
	}
	return checkPeerVersion(m.SoftwareVersion, role)
}

type attachmentControl interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func validateAttach(m message) error {
	if m.Action != "" && m.Action != "existing" {
		return fmt.Errorf("invalid attach action")
	}
	if (m.NoLogin || m.NoProfile || len(m.Argv) != 0) && m.Action != "" {
		return fmt.Errorf("startup options require a new shell")
	}
	if m.Term == "" || len(m.Term) > 128 || strings.ContainsAny(m.Term, "\x00\r\n\x1b") {
		return fmt.Errorf("invalid TERM")
	}
	return nil
}
