package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Control metadata uses bounded frames. Approved file streams switch to
// SFTP after their metadata handshake.

const requestTimeout = 10 * time.Second

type sessionTarget struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	Hosted   bool   `json:"hosted,omitempty"`
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

type message struct {
	ResourceRoute    []resourceHop       `json:"resource_route,omitempty"`
	NativeTargets    bool                `json:"native_targets,omitempty"`
	RetainedShells   []retainedShell     `json:"retained_shells,omitempty"`
	Owner            string              `json:"owner,omitempty"`
	TreeKey          string              `json:"tree_key,omitempty"`
	TTL              time.Duration       `json:"ttl,omitempty"`
	PinUntil         time.Time           `json:"pin_until,omitzero"`
	TerminalOffset   uint64              `json:"terminal_offset,omitempty"`
	OutputIncomplete bool                `json:"output_incomplete,omitempty"`
	DaemonShutdown   bool                `json:"daemon_shutdown,omitempty"`
	Reason           string              `json:"reason,omitempty"`
	Handoff          *clientHandoff      `json:"handoff,omitempty"`
	SourceHost       string              `json:"source_host,omitempty"`
	MachineID        string              `json:"machine_id,omitempty"`
	Pinned           bool                `json:"pinned,omitempty"`
	LocalRequest     *localDaemonRequest `json:"local_request,omitempty"`
	Instance         string              `json:"instance,omitempty"`
	Token            string              `json:"token,omitempty"`
	HandoffToken     string              `json:"handoff_token,omitempty"`
	ApprovalID       string              `json:"approval_id,omitempty"`
	Directory        string              `json:"directory,omitempty"`
	HomeDirectory    string              `json:"home_directory,omitempty"`
	AgeSeconds       *int64              `json:"age_seconds,omitempty"`
	Targets          []sessionTarget     `json:"targets,omitempty"`
	SoftwareVersion  string              `json:"software_version,omitempty"`
	Operations       []operation         `json:"operations,omitempty"`
	Action           string              `json:"action,omitempty"`
	Type             string              `json:"type"`
	Cols             uint16              `json:"cols,omitempty"`
	Rows             uint16              `json:"rows,omitempty"`
	Color            bool                `json:"color,omitempty"`
	NoProfile        bool                `json:"no_profile,omitempty"`
	NoLogin          bool                `json:"no_login,omitempty"`
	Term             string              `json:"term,omitempty"`
	PID              int                 `json:"pid,omitempty"`
	Foreground       string              `json:"foreground,omitempty"`
	Attached         bool                `json:"attached,omitempty"`
	Name             string              `json:"name,omitempty"`
	Shell            string              `json:"shell,omitempty"`
	Size             int64               `json:"size,omitempty"`
	Output           string              `json:"output,omitempty"`
	Error            string              `json:"error,omitempty"`
	Code             int                 `json:"code,omitempty"`
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

func writeMessage(w io.Writer, m message) error {
	body, err := json.Marshal(m)
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

func readControlJSON(r io.Reader, target any) error {
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
	return json.Unmarshal(body, target)
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
	if sameRelease(version, peer) {
		return nil
	}
	if peer == "" {
		peer = "unknown (older build)"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s. Managed processes must use the same full release", version, role, peer)
}

type attachmentControl interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	SetDeadline(time.Time) error
}

func clientAttachment(c attachmentControl) error {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	if err := writeMessage(c, message{Type: "attach_control", SoftwareVersion: version}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return fmt.Errorf("SSH shell handshake failed: %w", err)
	}
	if err := checkPeerVersion(m.SoftwareVersion, "shell daemon"); err != nil {
		return err
	}
	if m.Type != "attach_control_ready" {
		return fmt.Errorf("incompatible sshore server: %s %s", m.Type, m.Error)
	}
	return nil
}

func acceptAttachment(c attachmentControl, endpoints *terminalEndpoints) (net.Conn, message, error) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	request, err := readMessage(c)
	if err != nil {
		return nil, request, err
	}
	terminal, err := endpoints.take(request.Token)
	return terminal, request, err
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

// Terminal endpoints are authenticated by the daemon before this handshake.
// A short-lived, single-use token binds the raw endpoint to one attachment.
type terminalEndpoints struct {
	mu      sync.Mutex
	pending map[string]*terminalEndpoint
}
type terminalEndpoint struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *terminalEndpoint) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
func (r *terminalEndpoints) serve(c net.Conn, request message) error {
	if err := checkPeerVersion(request.SoftwareVersion, "terminal client"); err != nil {
		return err
	}
	endpoint := &terminalEndpoint{Conn: c, done: make(chan struct{})}
	defer endpoint.Close()
	token := rand.Text()
	r.mu.Lock()
	if len(r.pending) >= 8 {
		r.mu.Unlock()
		return fmt.Errorf("too many pending terminal endpoints")
	}
	if r.pending == nil {
		r.pending = make(map[string]*terminalEndpoint)
	}
	r.pending[token] = endpoint
	r.mu.Unlock()
	timer := time.AfterFunc(5*time.Second, func() {
		r.mu.Lock()
		pending := r.pending[token] == endpoint
		if pending {
			delete(r.pending, token)
		}
		r.mu.Unlock()
		if pending {
			endpoint.Close()
		}
	})
	defer timer.Stop()
	defer func() { r.mu.Lock(); delete(r.pending, token); r.mu.Unlock() }()
	if err := writeMessage(c, message{Type: "terminal_ready", SoftwareVersion: version, Token: token}); err != nil {
		return err
	}
	<-endpoint.done
	return nil
}
func (r *terminalEndpoints) take(token string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.pending[token]
	if c == nil {
		return nil, fmt.Errorf("terminal endpoint expired or already claimed")
	}
	delete(r.pending, token)
	return c, nil
}
