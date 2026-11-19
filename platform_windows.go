package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

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

// The terminal owns a kill-on-close job; Terminal.Close ends its process tree.
func stopSpawnTerminal(pid int, start string) error { return nil }

func lockResourceState(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

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

func prepareFTPPaths(o *ftpOptions) (func(), error) {
	cleanup := func() {}
	if o.Mountpoint != "" {
		if len(o.Mountpoint) == 2 && o.Mountpoint[1] == ':' {
			return cleanup, nil
		}
		p, e := absoluteLocalPath(o.Mountpoint)
		o.Mountpoint = p
		return cleanup, e
	}
	drives, e := windows.GetLogicalDrives()
	if e != nil {
		return cleanup, e
	}
	for c := byte('Z'); c >= 'D'; c-- {
		if drives&(1<<uint(c-'A')) == 0 {
			o.Mountpoint = string(c) + ":"
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

func requestOperations(command string, _ []operation) error {
	return serverCommand([]string{command})
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

func hostedExitHistory() []exitView { return []exitView{} }

func hostedShells(string) ([]shellView, error) { return nil, nil }

func registerPlatformDaemonRoutes(*http.ServeMux, *daemonTasks) {}

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

func resetRuntimeState([]string) error { return nil }

func validateInstallTarget(target string) error {
	for _, path := range []string{filepath.Dir(target), target} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && path == target {
			continue
		}
		if err != nil {
			return err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(name)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("installation path must not be a reparse point")
		}
		if info.Mode()&os.ModeSymlink != 0 || path == target && !info.Mode().IsRegular() || path != target && !info.IsDir() {
			return fmt.Errorf("installation target must be a regular executable in a real directory")
		}
	}
	return nil
}

//go:embed install.ps1
var windowsInstaller string

func runLocalInstaller(target string) error {
	// Start the installer in a separate console so this executable can exit before replacement.
	f, err := os.CreateTemp("", "sshore-install-*.ps1")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(windowsInstaller); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	err = startWindowsInstaller(f.Name(), target)
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	fmt.Fprintln(os.Stderr, "Continue setup in the installer window.")
	return nil
}

func startWindowsInstaller(path, target string) error {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		return err
	}
	file := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	invocation := file
	if target != "" {
		invocation += " -Path '" + strings.ReplaceAll(target, "'", "''") + "'"
	}
	script := fmt.Sprintf("Wait-Process -Id %d -ErrorAction SilentlyContinue; try { & %s } finally { Remove-Item -LiteralPath %s; Read-Host 'Press Enter to close' }", os.Getpid(), invocation, file)
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{powershell, "-NoProfile", "-Command", script}))
	if err != nil {
		return err
	}
	// Let the new console supply its own input and output handles.
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	app, err := windows.UTF16PtrFromString(powershell)
	if err != nil {
		return err
	}
	if err := windows.CreateProcess(app, cmdline, nil, nil, false, windows.CREATE_NEW_CONSOLE, nil, nil, &si, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}
