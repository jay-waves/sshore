package tmux

import "github.com/jay-waves/sshore/internal/sys"

func (t *Task[A, T]) Processes() (group int, processes []sys.Process) {
	group, _ = t.terminal.ForegroundGroup()
	return group, t.terminal.TaskProcesses()
}
