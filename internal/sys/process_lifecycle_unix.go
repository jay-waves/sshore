//go:build linux || (darwin && arm64)

package sys

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Catch HUP for the entire account daemon lifetime, including intervals with
// no hosted shells. Do not use SIG_IGN, which would survive child exec.
func ProtectDaemonHangups() func() {
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	return func() { signal.Stop(hangups) }
}

func DetachWorker(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func DetachAccountDaemon(c *exec.Cmd) { DetachWorker(c) }

func WaitForDaemonShutdown(_ context.Context, _ int, shutdown func() error) error {
	// Releasing the account lock is sufficient for replacement on Unix.
	return shutdown()
}
