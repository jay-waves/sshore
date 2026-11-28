package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/tmux"
)

func hostedShellEnvironment(source []string, term, instance, executable string) []string {
	env := append(append([]string(nil), source...), "TERM="+term, "SSHORE_SOCKET="+instance)
	if executable != "" {
		env = append(env, "PATH="+filepath.Dir(executable)+string(os.PathListSeparator)+shell.EnvValue(source, "PATH"))
	}
	return env
}

func (s *tmuxTask) shellLaunch(m message) (tmux.Launch, error) {
	source := s.environment
	if source == nil {
		source = os.Environ()
	}
	path := shell.DefaultPath(source, s.hosted)
	var env []string
	if s.hosted {
		exe, _ := os.Executable()
		env = hostedShellEnvironment(source, m.Term, s.record.UID, exe)
	} else {
		env = localShellEnvironment(source, m.Term, s.record.identity())
	}
	path, args, err := shell.Command(path, m.Argv, m.NoLogin, m.NoProfile)
	if err != nil {
		return tmux.Launch{}, err
	}
	if s.cgroupErr != nil {
		return tmux.Launch{}, s.cgroupErr
	}
	name := filepath.Base(path)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	}
	return tmux.Launch{
		Config: pty.Config{Path: path, Args: args, Env: shell.StartupEnv(path, env, m.NoProfile),
			Dir: s.record.Directory, TaskCgroup: s.cgroup},
		Process:         tmux.ProcessInfo{Shell: name, ProgramLine: shell.FormatCommand(m.Argv)},
		ReportDirectory: runtime.GOOS == "windows" && !s.hosted,
	}, nil
}

func localShellEnvironment(source []string, term, identity string) []string {
	env := make([]string, 0, len(source)+2)
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "SSHORE_SOCKET", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "SSHORE_LOCAL_SESSION", "TERM":
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM="+term, "SSHORE_LOCAL_SESSION="+identity)
	return env
}
