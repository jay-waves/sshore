package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func writeStateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeStateFile(path, data)
}

// Replace metadata only after a complete private write. File transfers use
// no-overwrite publication instead; pending approvals use exclusive creation.
func writeStateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = privateState(f.Name(), false); err != nil {
		return err
	}
	if err = writeFull(f, data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func stateDir(name string) (string, error) {
	root := os.Getenv("SSHORE_STATE_DIR")
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(root, "sshore")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err = privateState(root, true); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err = privateState(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}
