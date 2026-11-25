package sys

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome expands the current user's home prefix in a native path.
// It preserves spaces and quotes in paths already parsed by the caller.
func ExpandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || os.PathSeparator == '\\' && strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("home directory is not an absolute path")
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func Abs(path string) (string, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}
