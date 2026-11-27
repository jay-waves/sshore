package sys

import (
	"os"
	"path/filepath"
)

// StageSiblingFile gives the caller a closed, synced file on the destination
// filesystem. It owns cleanup until the path is returned.
func StageSiblingFile(target, pattern string, prepare func(*os.File) error) (staged string, err error) {
	f, err := os.CreateTemp(filepath.Dir(target), pattern)
	if err != nil {
		return "", err
	}
	defer func() {
		if staged == "" {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err = prepare(f); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}
