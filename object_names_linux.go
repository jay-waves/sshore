//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Inspect lifetime locks and labels, never contact the shells or signal tasks.
func occupiedShellNames() (map[string]bool, error) {
	names := map[string]bool{}
	dir, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{dir: true}
	processes, err := findSessionDaemons("/proc")
	if err != nil {
		return nil, err
	}
	for _, p := range processes {
		if p.socket != "" {
			dirs[filepath.Dir(p.socket)] = true
		}
	}
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".sock.lock") {
				continue
			}
			path := filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".lock"))
			live, err := sessionIdentityLive(path)
			if err != nil {
				return nil, err
			}
			if !live {
				continue
			}
			name, err := readSessionName(path)
			if err != nil {
				return nil, err
			}
			names[name] = true
		}
	}
	return names, nil
}
