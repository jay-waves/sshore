package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
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

func hostedShellEnvironment(source []string, term, instance, executable string) []string {
	env := append(append([]string(nil), source...), "TERM="+term, "SSHORE_SOCKET="+instance, "SSHORE_TREE_ID="+identityFromUID(instance))
	if executable != "" {
		env = append(env, "PATH="+filepath.Dir(executable)+string(os.PathListSeparator)+localEnvironmentValue(source, "PATH"))
	}
	return env
}

// The caller holds mu while starting and publishing the terminal.
func (s *shellTask) startManagedShell(m message) error {
	source := s.environment
	if source == nil {
		source = os.Environ()
	}
	var path string
	var env []string
	if s.hosted {
		path = cmp.Or(localEnvironmentValue(source, "SHELL"), "/bin/bash")
		if runtime.GOOS == "windows" {
			path = localShellPath(source)
		}
		exe, _ := os.Executable()
		env = hostedShellEnvironment(source, m.Term, s.record.UID, exe)
	} else {
		path = localShellPath(source)
		env = localShellEnvironment(source, m.Term, s.record.identity())
	}
	args, err := startupArgs(runtime.GOOS, path, m.NoLogin, m.NoProfile)
	if err != nil {
		return err
	}
	if s.cgroupErr != nil {
		return s.cgroupErr
	}
	t, err := pty.Start(pty.Config{
		Path: path, Args: args, Env: startupEnv(path, env, m.NoProfile),
		Dir: s.record.Directory, Size: shellSize(m), TaskCgroup: s.cgroup,
	})
	if err != nil {
		return err
	}
	s.terminal = t
	s.pid = t.PID()
	s.started = time.Now()
	s.shellName = filepath.Base(path)
	if runtime.GOOS == "windows" {
		s.shellName = strings.TrimSuffix(strings.ToLower(s.shellName), ".exe")
	}
	go s.pumpTerminal(t)
	return nil
}

func localShellEnvironment(source []string, term, identity string) []string {
	env := make([]string, 0, len(source)+2)
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_TTY", "SSHORE_LOCAL_SESSION", "SSHORE_TREE_ID", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity, "SSHORE_TREE_ID="+identity)
	return env
}
