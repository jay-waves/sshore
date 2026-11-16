package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/xtaci/smux"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/jay-waves/sshore/internal/tmux"
)

func localShellPath(env []string) string {
	path := localEnvironmentValue(env, "SHELL")
	if path == "" {
		path = localEnvironmentValue(env, "COMSPEC")
	}
	if path == "" {
		path = "cmd.exe"
	}
	return path
}

func redrawLocalTerminal(t *tmux.Terminal) { tmux.RequestRedraw(t, t.PID()) }

func detachLocalDaemon(cmd *exec.Cmd) {
	// The worker only uses pipes and sockets; the shell gets its own ConPTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func spawnProcessIdentity(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return "", err
	}
	if state != uint32(windows.WAIT_TIMEOUT) {
		return "", fmt.Errorf("parent shell exited")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", created.HighDateTime, created.LowDateTime), nil
}

// The terminal owns a kill-on-close job; Terminal.Close ends its process tree.
func stopSpawnTerminal(pid int, start string) error { return nil }

func detachResourceWorker(c *exec.Cmd) {
	detachLocalDaemon(c)
}
func lockResourceReadState(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func lockResourceState(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.mountpoint != "" {
		if len(o.mountpoint) == 2 && o.mountpoint[1] == ':' {
			return cleanup, nil
		}
		p, e := filepath.Abs(o.mountpoint)
		o.mountpoint = p
		return cleanup, e
	}
	drives, e := windows.GetLogicalDrives()
	if e != nil {
		return cleanup, e
	}
	for c := byte('Z'); c >= 'D'; c-- {
		if drives&(1<<uint(c-'A')) == 0 {
			o.mountpoint = string(c) + ":"
			return cleanup, nil
		}
	}
	return cleanup, fmt.Errorf("no free drive letter; specify a mountpoint")
}

func resourceLockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

func occupiedShellNames() (map[string]bool, error) { return map[string]bool{}, nil }

// Private validates the path type and applies a protected current-user DACL.
func privateStatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("invalid state path: %s", path)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func serverCommand(args []string) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote shell", args[0])
}

func approvedRemoteTransfer(st *smux.Stream, m message) {
	_ = writeMessage(st, message{Type: "error", Error: "file serving requires Linux"})
}

func localSessionInventory(details ...bool) (inventory, error) {
	inv := emptyInventory()
	var e error
	inv.Peers, e = collectPeerViews()
	return inv, e
}

func systemMachineID() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("MachineGuid")
	return value
}

func hostedShellInventory(detailed bool) (inventory, error) {
	return emptyInventory(), nil
}

func handoffRemoteSpawn(o options) (bool, error) { return false, nil }
