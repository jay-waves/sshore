package term

import (
	"path/filepath"
	"unicode/utf8"
)

const directoryPrefix = "9;9;"

// DirectoryReport consumes OSC 9;9 on the shell host, sharing the control
// sequence boundaries already decoded by InputState.
type DirectoryReport struct {
	payload   []byte
	capturing bool
	latest    string
}

// Observe returns the most recent valid report completed in data.
func (r *DirectoryReport) Observe(state *InputState, data []byte) string {
	r.latest = ""
	state.observe(data, r)
	return r.latest
}

func (r *DirectoryReport) advance(previous, next controlState, c byte) {
	switch {
	case previous == controlEscape && next == controlString:
		r.payload = r.payload[:0]
		r.capturing = true
	case !r.capturing:
	case previous == controlStringEscape && c == '\\', previous == controlString && c == 7:
		if path := directoryPath(r.payload); path != "" {
			r.latest = path
		}
		r.capturing = false
	case next == controlText || previous == controlStringEscape:
		r.capturing = false
	case next == controlString:
		n := len(r.payload)
		// A Windows path can occupy at most 32767 UTF-16 code units.
		if c < 0x20 || c == 0x7f || n == len(directoryPrefix)+3*32767+2 ||
			n < len(directoryPrefix) && c != directoryPrefix[n] {
			r.capturing = false
			return
		}
		r.payload = append(r.payload, c)
	}
}

func directoryPath(payload []byte) string {
	if len(payload) <= len(directoryPrefix) {
		return ""
	}
	path := payload[len(directoryPrefix):]
	if !utf8.Valid(path) {
		return ""
	}
	if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
		path = path[1 : len(path)-1]
	}
	if dir := string(path); filepath.IsAbs(dir) {
		return dir
	}
	return ""
}
