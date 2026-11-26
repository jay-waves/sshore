package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/sys/windows"
)

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
	fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info, "Continue setup in the installer window."))
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
