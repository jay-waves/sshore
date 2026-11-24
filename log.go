package main

import (
	"io"
	"strings"
	"sync"

	"github.com/jay-waves/sshore/internal/termui"
)

// logStream turns diagnostic output into individually identifiable log lines.
type logStream struct {
	mu          sync.Mutex
	out         io.Writer
	id, pending string
	level       termui.Color
}

func (s *logStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending += string(p)
	for {
		line, rest, ok := strings.Cut(s.pending, "\n")
		if !ok {
			break
		}
		termui.Log(s.out, s.id, s.level, "%s", strings.TrimSuffix(line, "\r"))
		s.pending = rest
	}
	return len(p), nil
}

func (s *logStream) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != "" {
		termui.Log(s.out, s.id, s.level, "%s", s.pending)
		s.pending = ""
	}
}
