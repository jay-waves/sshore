//go:build integration

package tests_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsInstallWaitsForExecutable(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	if binary == "" {
		t.Skip("set SSHORE_TEST_WINDOWS_CLIENT to the built Windows executable")
	}
	dir := t.TempDir()
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(dir, "state"))
	target := filepath.Join(dir, "sshore.exe")
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0700); err != nil {
		t.Fatal(err)
	}

	// A reader can keep the old executable from being replaced without
	// preventing the installer from running it to verify its version.
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	locked := os.NewFile(uintptr(handle), target)
	defer locked.Close()
	previous, err := locked.Stat()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cmd := exec.CommandContext(ctx, binary, "install", "--apply", "here:"+target)
	done := make(chan struct{})
	var output []byte
	var installErr error
	go func() {
		output, installErr = cmd.CombinedOutput()
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Keep the file locked after the installer has staged its replacement,
	// then release it while that same invocation is still running.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if matches, _ := filepath.Glob(filepath.Join(dir, ".sshore-update-*.exe")); len(matches) != 0 {
			break
		}
		select {
		case <-done:
			t.Fatalf("installer exited before the lock was released: %v\n%s", installErr, output)
		case <-ctx.Done():
			t.Fatal("installer did not stage a replacement")
		case <-tick.C:
		}
	}
	select {
	case <-done:
		t.Fatalf("installer exited while the executable was locked: %v\n%s", installErr, output)
	case <-time.After(500 * time.Millisecond):
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if installErr != nil {
		t.Fatalf("installer did not recover after release: %v\n%s", installErr, output)
	}
	current, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(previous, current) {
		t.Fatal("installed executable was not replaced")
	}
}
