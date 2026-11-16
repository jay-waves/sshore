//go:build windows

package files

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/winfsp/go-winfsp"
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

type ftpBlockingWindowsOpen struct {
	ftpWindowsBehaviour
	entered chan string
	release chan struct{}
}

func (b *ftpBlockingWindowsOpen) Open(_ *winfsp.FileSystemRef, name string, _, _ uint32, _ *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	b.entered <- name
	<-b.release
	if name == "a" {
		return 1, nil
	}
	return 2, nil
}
func TestFTPWindowsConcurrentOpens(t *testing.T) {
	b := &ftpBlockingWindowsOpen{entered: make(chan string, 2), release: make(chan struct{})}
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}, names: make(map[uintptr]string)}
	done := make(chan struct{}, 2)
	defer func() { close(b.release); <-done; <-done }()
	for _, name := range []string{"a", "b"} {
		go func() { _, _ = a.Open(nil, name, 0, 0, nil); done <- struct{}{} }()
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-b.entered:
		case <-timer.C:
			t.Fatal("unrelated opens serialized behind remote I/O")
		}
	}
}

type ftpBlockingWindowsClose struct {
	ftpBlockingWindowsOpen
	closing     chan struct{}
	finishClose chan struct{}
}

func (b *ftpBlockingWindowsClose) Close(_ *winfsp.FileSystemRef, _ uintptr) {
	close(b.closing)
	<-b.finishClose
}
func TestFTPWindowsCloseDoesNotBlockOpen(t *testing.T) {
	b := &ftpBlockingWindowsClose{
		ftpBlockingWindowsOpen: ftpBlockingWindowsOpen{entered: make(chan string, 1), release: make(chan struct{})},
		closing:                make(chan struct{}), finishClose: make(chan struct{}),
	}
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}, names: map[uintptr]string{3: "closed"}}
	done := make(chan struct{}, 2)
	go func() { a.Close(nil, 3); done <- struct{}{} }()
	<-b.closing
	go func() { _, _ = a.Open(nil, "a", 0, 0, nil); done <- struct{}{} }()
	defer func() { close(b.finishClose); close(b.release); <-done; <-done }()
	select {
	case <-b.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("remote close blocked an unrelated open")
	}
}

type ftpWindowsInfoOnly struct {
	ftpWindowsBehaviour
	calls int
}

func (b *ftpWindowsInfoOnly) GetFileInfo(_ *winfsp.FileSystemRef, _ uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	b.calls++
	info.FileSize = 42
	return nil
}
func TestFTPWindowsSyntheticTimes(t *testing.T) {
	b := &ftpWindowsInfoOnly{}
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}, names: map[uintptr]string{1: "file"}}
	var info winfsp.FSP_FSCTL_FILE_INFO
	if err := a.SetBasicInfo(nil, 1, winfsp.SetBasicInfoCreationTime|winfsp.SetBasicInfoChangeTime, 0, 1, 0, 0, 1, &info); err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 || info.FileSize != 42 {
		t.Fatalf("calls=%d info=%+v", b.calls, info)
	}
}

func TestFTPWindowsMetadataInvalidation(t *testing.T) {
	backend := testFTPBackend(t)
	fs := &ftpWindowsFS{backend: backend}
	file, err := fs.OpenFile("file", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	checkSize := func(want int64) {
		t.Helper()
		info, err := fs.Stat("file")
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != want {
			t.Fatalf("size=%d want %d", info.Size(), want)
		}
	}
	checkSize(0)
	if _, err := file.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	checkSize(3)
	if err := file.Truncate(1); err != nil {
		t.Fatal(err)
	}
	checkSize(1)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename("file", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat("file"); !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		t.Fatalf("stale rename: %v", err)
	}
	if _, err := fs.Stat("renamed"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat("renamed"); !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		t.Fatalf("stale remove: %v", err)
	}
}

func TestFTPWindowsHandleStatPolling(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	fs := &ftpWindowsFS{backend: backend}
	file, err := fs.OpenFile("polled", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for i := 0; i < 100; i++ {
		if _, err = file.Stat(); err != nil {
			t.Fatal(err)
		}
	}
	wire.mu.Lock()
	requests := wire.totals[8].Started
	wire.mu.Unlock()
	if requests != 1 {
		t.Fatalf("handle polling sent %d FSTAT requests", requests)
	}
	if _, err = file.Write([]byte("changed")); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != 7 {
		t.Fatalf("stale size after write: %v %v", info, err)
	}
}
