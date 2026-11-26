// Package shell prepares managed shell and program launches for the target OS.
package shell

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ValidStartupPath accepts absolute and explicitly relative paths on either supported OS.
func ValidStartupPath(path string) bool {
	if len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return false
	}
	for _, prefix := range []string{"/", `\\`, "./", "../", `.\`, `..\`} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return len(path) >= 3 && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", rune(path[0])) &&
		path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

// Command selects an explicit program or builds the default shell's arguments.
func Command(defaultPath string, argv []string, noLogin, noProfile bool) (string, []string, error) {
	if len(argv) != 0 {
		return argv[0], argv[1:], nil
	}
	args, err := startupArgs(runtime.GOOS, defaultPath, noLogin, noProfile)
	return defaultPath, args, err
}

// ResolveStartupDirectory resolves a validated path using the target OS.
func ResolveStartupDirectory(base, directory string) (string, error) {
	if directory == "" {
		return base, nil
	}
	if !ValidStartupPath(directory) {
		return "", fmt.Errorf("invalid startup directory %q", directory)
	}
	if filepath.IsAbs(directory) {
		return filepath.Clean(directory), nil
	}
	relative := filepath.ToSlash(directory)
	if strings.HasPrefix(relative, "./") || strings.HasPrefix(relative, "../") {
		return filepath.Join(base, directory), nil
	}
	return "", fmt.Errorf("startup directory %q must be absolute or explicitly relative on the target", directory)
}

// startupArgs selects startup mode and optional directory reporting.
func startupArgs(platform, path string, noLogin, noProfile bool) ([]string, error) {
	name := filepath.Base(path)
	if platform == "windows" {
		if noLogin {
			return nil, fmt.Errorf("--no-login requires a Unix shell; use --no-profile to skip Windows shell startup configuration")
		}
		name = strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/")))
		if !noProfile {
			if name == "pwsh" || name == "pwsh.exe" {
				return []string{"-NoExit", "-Command", pwshDirectoryReporting}, nil
			}
			return nil, nil
		}
		switch name {
		case "pwsh", "pwsh.exe", "powershell", "powershell.exe":
			return []string{"-NoProfile"}, nil
		case "cmd", "cmd.exe":
			return []string{"/D"}, nil
		default:
			return nil, fmt.Errorf("--no-profile is unsupported for shell %q", path)
		}
	}
	if noProfile {
		switch name {
		case "bash":
			return []string{"--noprofile", "--norc", "-i"}, nil
		case "zsh":
			return []string{"-f", "-i"}, nil
		case "fish":
			return []string{"--no-config", "-i"}, nil
		case "sh":
			return []string{"-i"}, nil
		default:
			return nil, fmt.Errorf("--no-profile is unsupported for shell %q", path)
		}
	}
	if noLogin {
		return []string{"-i"}, nil
	}
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ash", "ksh":
		return []string{"-l", "-i"}, nil
	default:
		return nil, fmt.Errorf("login startup is unsupported for shell %q; use --no-login for ordinary interactive startup", path)
	}
}

// -Command runs after the user's profiles in the same interactive runspace.
//
//go:embed pwsh_directory.ps1
var pwshDirectoryReporting string

// StartupEnv preserves inherited variables except sh's optional startup hook.
func StartupEnv(path string, env []string, noProfile bool) []string {
	if runtime.GOOS != "linux" || !noProfile || filepath.Base(path) != "sh" {
		return env
	}
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, "ENV=") {
			filtered = append(filtered, item)
		}
	}
	return filtered
}
