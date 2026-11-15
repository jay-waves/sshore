package mount

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/xtaci/smux"

	"github.com/jay-waves/sshore/internal/releaseversion"
	"github.com/jay-waves/sshore/internal/transport"
)

const (
	control byte = iota
	input
	output
)

type message struct {
	Type            string `json:"type"`
	SoftwareVersion string `json:"software_version,omitempty"`
	Error           string `json:"error,omitempty"`
	Directory       string `json:"directory,omitempty"`
	Name            string `json:"name,omitempty"`
	Data            []byte `json:"-"`
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

func checkPeerVersion(local, peer, role string) error {
	if releaseversion.Compatible(local, peer) {
		return nil
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%s; reconnect to update or recover", local, role, peer)
}
func muxConfig() *smux.Config { return transport.MuxConfig() }
