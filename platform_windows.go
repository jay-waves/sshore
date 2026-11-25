package main

import (
	"context"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

func protectDaemonHangups() func() { return func() {} }

func detachDaemon(cmd *exec.Cmd) {
	// The worker only uses pipes and sockets; the shell gets its own ConPTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func detachAccountDaemon(cmd *exec.Cmd) {
	detachDaemon(cmd)
	// OpenSSH closes its session job when the setup command exits.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
}

// The terminal owns a kill-on-close job; Terminal.Close ends its process tree.
func reportBootstrapCgroup() {}

func daemonTaskCgroup() (string, error) { return "", nil }

// Retain a wait-only handle across authenticated shutdown. The owner lock can be
// released before Windows unloads the daemon's executable.
func completeDaemonShutdown(ctx context.Context, pid int, shutdown func() error) error {
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

func systemMachineID() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("MachineGuid")
	return value
}

// checkPlatform uses the real kernel version, independent of manifest shims.
func checkPlatform() error {
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 || (version.MajorVersion == 10 && version.BuildNumber < 22000) {
		return fmt.Errorf("Windows 11 / Server 2025 or newer required")
	}
	return nil
}
