package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jay-waves/sshore/internal/termui"
)

func openDaemonLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, "local.log")
	log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := privateStatePath(path, false); err != nil {
		_ = log.Close()
		return nil, err
	}
	return log, nil
}

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
