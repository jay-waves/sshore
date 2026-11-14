//go:build windows

package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFTPWindowsNames(t *testing.T) {
	for _, name := range []string{`\中文 folder\file.txt`, `\`, `folder/file.txt`} {
		if _, e := ftpWindowsPath(name); e != nil {
			t.Errorf("valid %q: %v", name, e)
		}
	}
	for _, name := range []string{`\..\outside`, `CON`, `file.`, `file `, `a:b`} {
		if _, e := ftpWindowsPath(name); !errors.Is(e, windows.ERROR_INVALID_NAME) {
			t.Errorf("invalid %q: %v", name, e)
		}
	}
}
func TestFTPWindowsErrors(t *testing.T) {
	for _, tc := range []struct {
		in   error
		want windows.NTStatus
	}{{os.ErrNotExist, windows.STATUS_OBJECT_NAME_NOT_FOUND}, {syscall.EROFS, windows.STATUS_MEDIA_WRITE_PROTECTED}, {syscall.ENOTDIR, windows.STATUS_NOT_A_DIRECTORY}, {syscall.EISDIR, windows.STATUS_FILE_IS_A_DIRECTORY}, {os.ErrClosed, windows.STATUS_INVALID_HANDLE}} {
		if e := ftpWindowsError(tc.in); !errors.Is(e, tc.want) {
			t.Errorf("%v: %v, want %v", tc.in, e, tc.want)
		}
	}
}

func TestWinFspInstallDiagnostics(t *testing.T) {
	cases := []struct {
		findErr, loadErr error
		want             string
	}{
		{windows.ERROR_FILE_NOT_FOUND, nil, "https://winfsp.dev/"},
		{windows.ERROR_ACCESS_DENIED, nil, "cannot inspect"},
		{nil, windows.ERROR_MOD_NOT_FOUND, "installed but could not be loaded"},
	}
	for _, tc := range cases {
		called := false
		e := checkWinFsp(func() (string, error) { return "bin", tc.findErr }, func() error { called = true; return tc.loadErr })
		if e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Fatalf("%v", e)
		}
		if called != (tc.findErr == nil) {
			t.Fatal("unexpected DLL load")
		}
		cause := tc.findErr
		if cause == nil {
			cause = tc.loadErr
		}
		if !errors.Is(e, cause) {
			t.Fatal("lost underlying failure")
		}
	}
	if e := checkWinFsp(func() (string, error) { return "bin", nil }, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
}
