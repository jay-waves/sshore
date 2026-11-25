package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

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
const pwshDirectoryReporting = `
& {
    if ($ExecutionContext.SessionState.LanguageMode -ne 'FullLanguage') { return }
    $reportDirectory = [System.EventHandler[System.Management.Automation.LocationChangedEventArgs]] {
        if ($PWD.Provider.Name -eq 'FileSystem') {
            $esc = [char]27
            [Console]::Write("$esc]9;9;" + '"' + $PWD.ProviderPath + '"' + "$esc\")
        }
    }
    $invocation = $ExecutionContext.SessionState.InvokeCommand
    $invocation.LocationChangedAction = [Delegate]::Combine($reportDirectory, $invocation.LocationChangedAction)
    $reportDirectory.Invoke($null, $null)
}`

// startupEnv preserves inherited variables except sh's optional startup hook.
func startupEnv(path string, env []string, noProfile bool) []string {
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
