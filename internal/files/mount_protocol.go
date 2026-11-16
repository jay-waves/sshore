package files

import (
	"fmt"
	"io"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/transport"
)

type MountApproval struct {
	Session string
	PID     int
	ID      string
}

type message struct {
	Approval *MountApproval `json:"mount_approval,omitempty"`

	Type            string `json:"type"`
	SoftwareVersion string `json:"software_version,omitempty"`
	Error           string `json:"error,omitempty"`
	Directory       string `json:"directory,omitempty"`
	Name            string `json:"name,omitempty"`
	Data            []byte `json:"-"`
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

func checkPeerVersion(local, peer, role string) error {
	if local != "" && local == peer {
		return nil
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s; reconnect to update or recover", local, role, peer)
}
func muxConfig() *smux.Config {
	config := transport.MuxConfig()
	// Mounts have separate data/notification streams and no terminal to compete
	// with. A larger data window avoids throttling pipelined SFTP on higher RTTs.
	config.MaxStreamBuffer = 4 * 1024 * 1024
	// Only data and notifications share this connection; reserve headroom for both.
	config.MaxReceiveBuffer = 3 * config.MaxStreamBuffer
	return config
}
