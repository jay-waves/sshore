package files

import (
	"runtime"

	"github.com/jay-waves/sshore/internal/sys"
)

// PrepareMountpoint resolves an explicit local path or allocates a default.
// Pass the validated task name for a background mount, or an empty name for a foreground mount.
// Call the returned cleanup function after using the mountpoint.
func PrepareMountpoint(path, name string) (string, func(), error) {
	if path == "" {
		return defaultMountpoint(name)
	}
	absolute, err := ResolveMountpoint(path)
	if err != nil {
		return "", nil, err
	}
	return absolute, func() {}, nil
}

// ResolveMountpoint resolves an explicit path without creating it.
func ResolveMountpoint(path string) (string, error) {
	if runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':' {
		return path, nil
	}
	return sys.Abs(path)
}
