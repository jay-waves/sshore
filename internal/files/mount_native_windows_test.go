package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
