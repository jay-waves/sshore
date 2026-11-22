//go:build windows

package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

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
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}}
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
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}}
	a.handles.Store(uintptr(3), ftpWindowsHandle{name: "closed"})
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
	a := &ftpWindowsAttributes{ftpWindowsBehaviour: b, backend: &ftpFS{}}
	a.handles.Store(uintptr(1), ftpWindowsHandle{name: "file"})
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

func TestFTPWindowsMetadataAccess(t *testing.T) {
	base := uint32(windows.FILE_OPEN << 24)
	for _, tc := range []struct {
		opts, access uint32
		want         bool
	}{
		{base, windows.FILE_READ_ATTRIBUTES, true},
		{base | windows.FILE_OPEN_REPARSE_POINT, windows.FILE_READ_ATTRIBUTES, true},
		{base | windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, true},
		{base, windows.FILE_READ_DATA, false},
		{base, windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES, false},
		{base, windows.FILE_READ_ATTRIBUTES | windows.DELETE, false},
		{base | windows.FILE_DELETE_ON_CLOSE, windows.FILE_READ_ATTRIBUTES, false},
		{windows.FILE_OPEN_IF << 24, windows.FILE_READ_ATTRIBUTES, false},
	} {
		if got := ftpWindowsMetadataOpen(tc.opts, tc.access); got != tc.want {
			t.Errorf("opts=%x access=%x: %v", tc.opts, tc.access, got)
		}
	}
}

func TestFTPWindowsMetadataLeases(t *testing.T) {
	backend, wire := testFTPBackendWithWire(t)
	if err := os.WriteFile(filepath.Join(filepath.FromSlash(backend.root), "file"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	pool := &ftpWindowsMetadataPool{backend: backend, entries: make(map[string]*ftpWindowsMetadataEntry)}
	defer pool.close()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			f, err := pool.acquire("file")
			if err == nil {
				_, err = f.Stat()
				_ = f.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	wire.mu.Lock()
	opens, stats := wire.totals[3].Started, wire.totals[8].Started
	wire.mu.Unlock()
	if opens != 1 || stats != 1 {
		t.Fatalf("concurrent attribute queries: OPEN=%d FSTAT=%d", opens, stats)
	}
	pinned, err := pool.acquire("file")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	lease := pinned.(*ftpWindowsMetadataLease)
	if _, err = pinned.Read(make([]byte, 1)); !errors.Is(err, windows.STATUS_ACCESS_DENIED) {
		t.Fatalf("metadata read: %v", err)
	}
	backend.cache.invalidateDirectory(backend.root)
	next, err := pool.acquire("file")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.(*ftpWindowsMetadataLease).entry == lease.entry {
		t.Fatal("retired handle reused")
	}
	if _, err = pinned.Stat(); err != nil {
		t.Fatalf("retirement closed an active lease: %v", err)
	}
	pool.close()
	if _, err = pinned.Stat(); !errors.Is(err, windows.STATUS_INVALID_HANDLE) {
		t.Fatalf("shutdown retained SFTP handle: %v", err)
	}
	if _, err = pool.acquire("file"); !errors.Is(err, windows.STATUS_INVALID_HANDLE) {
		t.Fatalf("open after shutdown: %v", err)
	}
}

func TestFTPWindowsMetadataInvalidationAndBounds(t *testing.T) {
	backend, _ := testFTPBackendWithWire(t)
	pool := &ftpWindowsMetadataPool{backend: backend, entries: make(map[string]*ftpWindowsMetadataEntry)}
	defer pool.close()
	create := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(filepath.FromSlash(backend.root), name), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	create("file")
	acquire := func() *ftpWindowsMetadataLease {
		t.Helper()
		f, err := pool.acquire("file")
		if err != nil {
			t.Fatal(err)
		}
		return f.(*ftpWindowsMetadataLease)
	}
	first := acquire()
	first.Close()
	for _, invalidate := range []func(){
		func() { backend.cache.beginMutation(backend.root + "/file")() },
		func() { backend.cache.invalidateDirectory(backend.root) },
		func() { backend.cache.invalidateRemote() },
		func() { pool.mu.Lock(); pool.entries["file"].expires = time.Now().Add(-time.Second); pool.mu.Unlock() },
	} {
		invalidate()
		next := acquire()
		if next.entry == first.entry {
			t.Fatal("invalidated handle reused")
		}
		next.Close()
		first = next
	}
	for i := 0; i < ftpWindowsMetadataHandles+2; i++ {
		name := fmt.Sprintf("bound-%d", i)
		create(name)
		f, err := pool.acquire(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	pool.mu.Lock()
	retained, all := len(pool.entries), len(pool.all)
	pool.mu.Unlock()
	if retained > ftpWindowsMetadataHandles || all > ftpWindowsMetadataHandles {
		t.Fatalf("unbounded descriptors: retained=%d all=%d", retained, all)
	}
}

func TestFTPNativeMetadataQueries(t *testing.T) {
	if os.Getenv("SSHORE_TEST_FTP_MOUNT") != "1" {
		t.Skip("set SSHORE_TEST_FTP_MOUNT=1 to use native WinFsp")
	}
	backend, wire := testFTPBackendWithWire(t)
	if err := os.WriteFile(filepath.Join(filepath.FromSlash(backend.root), "file"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(t.TempDir(), "mounted")
	mount, err := mountFTP(backend, point)
	if err != nil {
		t.Fatal(err)
	}
	defer mount.Unmount()
	target, err := windows.UTF16PtrFromString(filepath.Join(point, "file"))
	if err != nil {
		t.Fatal(err)
	}
	var pinned windows.Handle
	for i := 0; i < 20; i++ {
		h, err := windows.CreateFile(target, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		var info ftpNativeFileStandardInfo
		if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(h)
			t.Fatal(err)
		}
		if info.EndOfFile != 8 {
			t.Fatalf("size=%d", info.EndOfFile)
		}
		if i == 0 {
			pinned = h
			defer func() {
				if pinned != windows.InvalidHandle {
					windows.CloseHandle(pinned)
				}
			}()
		} else {
			windows.CloseHandle(h)
		}
	}
	wire.mu.Lock()
	opens, stats := wire.totals[3].Started, wire.totals[8].Started
	wire.mu.Unlock()
	if opens != 1 || stats != 1 {
		t.Fatalf("20 metadata opens: OPEN=%d FSTAT=%d", opens, stats)
	}
	// Data opens must bypass the attribute pool, including while a lease is pinned.
	data, err := os.ReadFile(filepath.Join(point, "file"))
	if err != nil || string(data) != "original" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	wire.mu.Lock()
	dataOpens := wire.totals[3].Started
	wire.mu.Unlock()
	if dataOpens <= opens {
		t.Fatal("data open reused attribute handle")
	}

	// Invalidating/expiring a cached entry must not close a still-pinned handle.
	backend.cache.invalidateRemote()
	var original ftpNativeFileStandardInfo
	if err := windows.GetFileInformationByHandleEx(pinned, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&original)), uint32(unsafe.Sizeof(original))); err != nil || original.EndOfFile != 8 {
		t.Fatalf("pinned handle after invalidation: size=%d err=%v", original.EndOfFile, err)
	}
	windows.CloseHandle(pinned)
	pinned = windows.InvalidHandle
	// This fixture hosts SFTP on Windows, whose os.Open lacks Unix rename sharing.
	// Wait for idle leases to expire before exercising the existing rename workflow.
	deadline := time.Now().Add(ftpWindowsMetadataHandleTTL + time.Second)
	for {
		wire.mu.Lock()
		idle := wire.totals[3].Started == wire.totals[4].Started
		wire.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle metadata handles did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Rename(filepath.Join(point, "file"), filepath.Join(point, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(point, "file"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(point, "file"))
	if err != nil || info.Size() != 3 {
		t.Fatalf("replacement stat=%v err=%v", info, err)
	}
	if err := mount.Unmount(); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	opened, closed := wire.totals[3].Started, wire.totals[4].Started
	wire.mu.Unlock()
	if opened != closed {
		t.Fatalf("unmount leaked remote handles: OPEN=%d CLOSE=%d", opened, closed)
	}
}

type ftpNativeFileStandardInfo struct {
	AllocationSize, EndOfFile int64
	NumberOfLinks             uint32
	DeletePending, Directory  uint8
	Reserved                  [2]byte
}
