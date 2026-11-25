package sys

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func ProtectDaemonHangups() func() { return func() {} }

func DetachWorker(cmd *exec.Cmd) {
	// The worker only uses pipes and sockets; the shell gets its own ConPTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func DetachAccountDaemon(cmd *exec.Cmd) {
	DetachWorker(cmd)
	// OpenSSH closes its session job when the setup command exits.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
}

// Retain a wait-only handle across authenticated shutdown. The owner lock can be
// released before Windows unloads the daemon's executable.
func WaitForDaemonShutdown(ctx context.Context, pid int, shutdown func() error) error {
	if pid == os.Getpid() {
		return shutdown()
	}
	if pid <= 0 {
		return fmt.Errorf("cannot wait for daemon exit: invalid daemon process ID")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("cannot wait for daemon exit: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := shutdown(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	milliseconds := max(0, time.Until(deadline).Milliseconds())
	status, err := windows.WaitForSingleObject(handle, uint32(milliseconds))
	if err != nil {
		return fmt.Errorf("wait for daemon exit: %w", err)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("daemon process did not exit before the shutdown deadline")
	}
	return nil
}
