package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/shell"
)

func hostedShellEnvironment(source []string, term, instance, executable string) []string {
	env := append(append([]string(nil), source...), "TERM="+term, "SSHORE_SOCKET="+instance, "SSHORE_TREE_ID="+identityFromUID(instance))
	if executable != "" {
		env = append(env, "PATH="+filepath.Dir(executable)+string(os.PathListSeparator)+shell.EnvValue(source, "PATH"))
	}
	return env
}

// The caller holds mu while starting and publishing the terminal.
func (s *shellTask) startManagedShell(m message) error {
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
		return err
	}
	if s.cgroupErr != nil {
		return s.cgroupErr
	}
	t, err := pty.Start(pty.Config{
		Path: path, Args: args, Env: shell.StartupEnv(path, env, m.NoProfile),
		Dir: s.record.Directory, Size: shellSize(m), TaskCgroup: s.cgroup,
	})
	if err != nil {
		return err
	}
	s.terminal = t
	s.pid = t.PID()
	s.started = time.Now()
	s.shellName = filepath.Base(path)
	s.program = len(m.Argv) != 0
	if s.program {
		s.programLine = shell.FormatCommand(m.Argv)
	}
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
