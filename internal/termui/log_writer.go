package termui

import (
	"io"
	"strings"
	"sync"
)

// LogWriter turns diagnostic output into individually identifiable log lines.
type LogWriter struct {
	mu          sync.Mutex
	out         io.Writer
	id, pending string
	level       Color
}

func NewLogWriter(out io.Writer, id string, level Color) *LogWriter {
	return &LogWriter{out: out, id: id, level: level}
}

func (s *LogWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending += string(p)
	for {
		line, rest, ok := strings.Cut(s.pending, "\n")
		if !ok {
			break
		}
		Log(s.out, s.id, s.level, "%s", strings.TrimSuffix(line, "\r"))
		s.pending = rest
	}
	return len(p), nil
}

func (s *LogWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != "" {
		Log(s.out, s.id, s.level, "%s", s.pending)
		s.pending = ""
	}
}
