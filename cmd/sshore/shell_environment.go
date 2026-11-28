package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jay-waves/sshore/internal/shell"
	"github.com/jay-waves/sshore/internal/tmux"
)

func (s *shellTask) processConfig() tmux.ProcessConfig {
	source := s.environment
	if source == nil {
		source = os.Environ()
	}
	config := tmux.ProcessConfig{
		Directory: s.record.Directory, TaskCgroup: s.registry.cgroup, SetupError: s.registry.cgroupErr,
		ReportDirectory: runtime.GOOS == "windows" && !s.hosted,
	}
	if s.hosted {
		executable, _ := os.Executable()
		config.Environment = hostedShellEnvironment(source, s.record.UID, executable)
		config.FallbackShell = "/bin/bash"
	} else {
		config.Environment = localShellEnvironment(source, s.record.identity())
	}
	return config
}

func hostedShellEnvironment(source []string, instance, executable string) []string {
	env := append(append([]string(nil), source...), "SSHORE_SOCKET="+instance)
	if executable != "" {
		path := filepath.Dir(executable)
		if inherited := shell.EnvValue(source, "PATH"); inherited != "" {
			path += string(os.PathListSeparator) + inherited
		}
		env = append(env, "PATH="+path)
	}
	return env
}

func localShellEnvironment(source []string, identity string) []string {
	env := make([]string, 0, len(source)+1)
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
	return append(env, "SSHORE_LOCAL_SESSION="+identity)
}
