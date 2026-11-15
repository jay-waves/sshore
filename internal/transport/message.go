package transport

import (
	"encoding/json"
	"fmt"
	"io"
)

// Message keeps metadata ownership with its caller while sharing frame rules.
// Binary payloads bypass JSON; control metadata must have a non-reserved type.
type Message interface {
	FrameType() string
	SetFrame(string, []byte)
}

func WriteMessage(w io.Writer, kind string, data []byte, metadata any) error {
	switch kind {
	case "input":
		return WriteFrame(w, 1, data)
	case "output":
		return WriteFrame(w, 2, data)
	default:
		body, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		return WriteFrame(w, 0, body)
	}
}

func ReadMessage(r io.Reader, metadata Message) error {
	kind, body, err := ReadFrame(r)
	if err != nil {
		return err
	}
	switch kind {
	case 1:
		metadata.SetFrame("input", body)
	case 2:
		metadata.SetFrame("output", body)
	default:
		if err := json.Unmarshal(body, metadata); err != nil {
			return err
		}
		switch metadata.FrameType() {
		case "", "input", "output", "chunk":
			return fmt.Errorf("invalid control type")
		}
	}
	return nil
}
