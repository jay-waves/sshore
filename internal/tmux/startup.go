package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jay-waves/sshore/internal/pty"
	"github.com/jay-waves/sshore/internal/shell"
)

// ProcessConfig describes the host environment for a session's first attachment.
type ProcessConfig struct {
	Directory, FallbackShell, TaskCgroup string
	Environment                          []string
	SetupError                           error
	ReportDirectory                      bool
}

type Startup struct {
	Argv               []string
	NoLogin, NoProfile bool
}

func (s Startup) configured() bool {
	return len(s.Argv) != 0 || s.NoLogin || s.NoProfile
}

func (t *Session) startLocked(startup Startup, size pty.Size, term string) error {
	config := t.config
	env := config.Environment
	if env == nil {
		env = os.Environ()
	}
	path, args, err := shell.Command(shell.DefaultPath(env, config.FallbackShell), startup.Argv, startup.NoLogin, startup.NoProfile)
	if err != nil {
		return err
	}
	if config.SetupError != nil {
		return config.SetupError
	}
	env = append(append([]string(nil), env...), "TERM="+term)
	terminal, err := pty.Start(pty.Config{
		Path: path, Args: args, Dir: config.Directory, Size: size,
		Env: shell.StartupEnv(path, env, startup.NoProfile), TaskCgroup: config.TaskCgroup,
	})
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	}
	t.stopTimer()
	t.terminal = terminal
	t.process = ProcessInfo{Shell: name, ProgramLine: shell.FormatCommand(startup.Argv)}
	// The child inherits its settings. The live session only needs its process state.
	t.config = ProcessConfig{Directory: config.Directory}
	go t.pumpOutput(config.ReportDirectory)
	return nil
}
