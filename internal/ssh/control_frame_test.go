package ssh

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestControlFrameLeavesBinaryPayloadUnread(t *testing.T) {
	const payload = "\x00\n\xff"
	for _, tc := range []struct {
		name  string
		frame string
		wrap  func(io.Reader) io.Reader
	}{
		{"buffered", "ready", func(r io.Reader) io.Reader { return bufio.NewReader(r) }},
		{"fragmented", strings.Repeat("x", 32), func(r io.Reader) io.Reader { return bufio.NewReaderSize(r, 16) }},
		{"unbuffered", "ready", func(r io.Reader) io.Reader { return struct{ io.Reader }{r} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := tc.wrap(strings.NewReader(tc.frame + "\n" + payload))
			frame, err := ReadControlFrame(stream, len(tc.frame))
			if err != nil || string(frame) != tc.frame {
				t.Fatalf("frame %q, error %v", frame, err)
			}
			if data, err := io.ReadAll(stream); err != nil || string(data) != payload {
				t.Fatalf("payload %q, error %v", data, err)
			}
		})
	}
}
