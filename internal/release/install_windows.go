package release

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func RunInstaller(target, _, script string) error {
	// Start the installer in a separate console so this executable can exit before replacement.
	f, err := os.CreateTemp("", "sshore-install-*.ps1")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(script); err != nil {
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
