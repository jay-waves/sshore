package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/xtaci/smux"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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

func detachMountDaemon(c *exec.Cmd) {
	detachLocalDaemon(c)
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

// Private validates the path type and applies a protected current-user DACL.
func privateStatePath(path string, directory bool) error {
	native, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(native, `\\?\`) && !strings.HasPrefix(native, `\\.\`) {
		if strings.HasPrefix(native, `\\`) {
			native = `\\?\UNC\` + native[2:]
		} else {
			native = `\\?\` + native
		}
	}
	name, err := windows.UTF16PtrFromString(native)
	if err != nil {
		return err
	}
	// Keep validation and ACL mutation on one object; never follow a reparse point.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "secure", Path: path, Err: err}
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return fmt.Errorf("invalid state path: %s", path)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	current, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	// Reapplying an inheritable directory DACL walks its children. Those native
	// security handles can prevent a concurrent atomic metadata replacement.
	// The auto-inherited bit does not change this protected, explicit ACL.
	want, got := sd.String(), current.String()
	if got == want || got == strings.Replace(want, "D:P", "D:PAI", 1) {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("secure state ACL: %w", err)
	}
	return nil
}

func serverCommand(args []string) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote shell", args[0])
}

func approvedRemoteTransfer(st *smux.Stream, m message) {
	_ = writeMessage(st, message{Type: "error", Error: "file serving requires Linux"})
}

func localSessionInventory(details ...bool) inventory {
	inv := emptyInventory()
	var e error
	inv.Peers, e = collectPeerViews()
	if e != nil {
		inv.Errors = append(inv.Errors, cleanText(e.Error()))
	}
	return inv
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

func hostedShellTasks() ([]selectedTask, error) { return nil, nil }

func newDaemonServices(tasks *daemonTasks) daemonServices { return nil }

func openHostedTransport(context.Context, options) (net.Conn, error) {
	return nil, fmt.Errorf("Windows does not host Unix shells")
}

func runHostedCommand(options) (int, error) {
	return 1, fmt.Errorf("Windows does not host Unix shells")
}

func handoffRemoteSpawn(o options) (bool, error) { return false, nil }

// checkPlatform uses the real kernel version, independent of manifest shims.
func checkPlatform() error {
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 || (version.MajorVersion == 10 && version.BuildNumber < 22000) {
		return fmt.Errorf("Windows 11 / Server 2025 or newer required")
	}
	return nil
}
