//go:build sshoretest

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lifecycle integration tests use isolated accounts, never the developer's daemon.
func daemonLocation() (string, error) {
	if !filepath.IsAbs(os.Getenv("SSHORE_STATE_DIR")) {
		return "", fmt.Errorf("sshoretest daemon requires an isolated absolute SSHORE_STATE_DIR")
	}
	return statePath("daemon")
}

func daemonDirectory() (string, error) {
	if _, err := daemonLocation(); err != nil {
		return "", err
	}
	return stateDir("daemon")
}

func daemonStateRoot(root string, explicit bool) (string, error) { return root, nil }

// Destructive reset fixtures must never reach the developer's release cache.
func resetCachePath() (string, error) {
	if _, err := daemonLocation(); err != nil {
		return "", err
	}
	root, err := statePath("")
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "release-cache"), nil
}
