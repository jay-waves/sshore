//go:build windows

package main

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestResourceHiddenConsoleInheritance(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestResourceConsoleChild$")
	detachResourceWorker(cmd)
	cmd.Env = append(os.Environ(), "SSHORE_TEST_CONSOLE_CHILD=worker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hidden console: %v\n%s", err, out)
	}
}

func TestResourceConsoleChild(t *testing.T) {
	role := os.Getenv("SSHORE_TEST_CONSOLE_CHILD")
	if role == "" {
		t.Skip("subprocess fixture")
	}
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if hwnd == 0 {
		t.Fatal("worker and askpass must share an allocated hidden console")
	}
	visible, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible").Call(hwnd)
	if visible != 0 {
		t.Fatal("background console is visible")
	}
	if role == "worker" {
		// OpenSSH launches a console-subsystem askpass executable without our Go
		// creation flags. Reproduce that child launch, not just the worker's flags.
		cmd := exec.Command(os.Args[0], "-test.run=^TestResourceConsoleChild$")
		cmd.Env = append(os.Environ(), "SSHORE_TEST_CONSOLE_CHILD=askpass")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("askpass console: %v\n%s", err, out)
		}
	}
}
