//go:build windows

package main

// WinFsp uses its native Go binding, without CGO. gofs manages Windows handle
// sharing and rename bookkeeping; the adapter translates remote errors and
// rejects names that Windows cannot represent rather than aliasing them.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

func validateFTPMountpoint(p string) error {
	if len(p) == 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		if _, e := os.Stat(p + `\`); e == nil {
			return fmt.Errorf("drive %s is already in use", p)
		}
		return nil
	}
	if _, e := os.Lstat(p); !os.IsNotExist(e) {
		return fmt.Errorf("Windows mountpoint must be an unused drive letter or a nonexistent directory")
	}
	return nil
}

type ftpWindowsMount struct {
	fs   *winfsp.FileSystem
	done chan struct{}
	once sync.Once
}

func (m *ftpWindowsMount) Wait() { <-m.done }
func (m *ftpWindowsMount) Unmount() error {
	m.once.Do(func() { m.fs.Unmount(); close(m.done) })
	return nil
}
func mountFTP(f *ftpFS, p string) (ftpMount, error) {
	opts := []winfsp.Option{winfsp.FileSystemName("sshore"), winfsp.CaseSensitive(true), winfsp.CasePreserveNames(true)}
	if f.readOnly {
		opts = append(opts, winfsp.Attributes(winfsp.FspFSAttributeReadOnlyVolume))
	}
	base := gofs.New(&ftpWindowsFS{backend: f}).(ftpWindowsBehaviour)
	adapter := &ftpWindowsAttributes{ftpWindowsBehaviour: base, backend: f, names: make(map[uintptr]string)}
	host, e := winfsp.Mount(adapter, p, opts...)
	if e != nil {
		return nil, fmt.Errorf("WinFsp mount failed: %w", e)
	}
	return &ftpWindowsMount{fs: host, done: make(chan struct{})}, nil
}

type ftpWindowsFS struct{ backend *ftpFS }

func ftpWindowsPath(p string) (string, error) {
	p = strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/")
	for _, part := range strings.Split(p, "/") {
		if part == "" {
			continue
		}
		if e := safeName(part); e != nil {
			return "", windows.ERROR_INVALID_NAME
		}
	}
	return p, nil
}
func ftpWindowsError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, io.EOF) {
		return io.EOF
	}
	switch ftpErrno(e) {
	case syscall.ENOENT:
		return windows.STATUS_OBJECT_NAME_NOT_FOUND
	case syscall.EACCES, syscall.EPERM:
		return windows.STATUS_ACCESS_DENIED
	case syscall.EEXIST:
		return windows.STATUS_OBJECT_NAME_COLLISION
	case syscall.EROFS:
		return windows.STATUS_MEDIA_WRITE_PROTECTED
	case syscall.EISDIR:
		return windows.STATUS_FILE_IS_A_DIRECTORY
	case syscall.ENOTDIR:
		return windows.STATUS_NOT_A_DIRECTORY
	case syscall.EBADF:
		return windows.STATUS_INVALID_HANDLE
	case syscall.EINVAL:
		return windows.STATUS_INVALID_PARAMETER
	case syscall.ENOTSUP:
		return windows.STATUS_NOT_SUPPORTED
	default:
		return windows.STATUS_IO_DEVICE_ERROR
	}
}

func (f *ftpWindowsFS) OpenFile(p string, flags int, perm os.FileMode) (gofs.File, error) {
	p, e := ftpWindowsPath(p)
	if e != nil {
		return nil, e
	}
	h, e := f.backend.open(p, flags, perm)
	if e != nil {
		return nil, ftpWindowsError(e)
	}
	return &ftpWindowsFile{ftpFile: h}, nil
}
func (f *ftpWindowsFS) Stat(p string) (os.FileInfo, error) {
	p, e := ftpWindowsPath(p)
	if e != nil {
		return nil, e
	}
	i, e := f.backend.stat(p)
	return i, ftpWindowsError(e)
}
func (f *ftpWindowsFS) Mkdir(p string, perm os.FileMode) error {
	p, e := ftpWindowsPath(p)
	if e != nil {
		return e
	}
	return ftpWindowsError(f.backend.mkdir(p, perm))
}
func (f *ftpWindowsFS) Remove(p string) error {
	p, e := ftpWindowsPath(p)
	if e != nil {
		return e
	}
	remote, e := f.backend.remotePath(p)
	if e != nil {
		return ftpWindowsError(e)
	}
	i, e := f.backend.client.Lstat(remote)
	if e != nil {
		return ftpWindowsError(e)
	}
	return ftpWindowsError(f.backend.remove(p, i.IsDir()))
}
func (f *ftpWindowsFS) Rename(a, b string) error {
	a, e := ftpWindowsPath(a)
	if e != nil {
		return e
	}
	b, e = ftpWindowsPath(b)
	if e != nil {
		return e
	}
	return ftpWindowsError(f.backend.rename(a, b))
}

type ftpWindowsFile struct{ *ftpFile }

func (f *ftpWindowsFile) Readdir(n int) ([]os.FileInfo, error) {
	list, e := f.ftpFile.Readdir(n)
	if e != nil {
		return nil, ftpWindowsError(e)
	}
	for _, i := range list {
		if e := safeName(i.Name()); e != nil {
			return nil, windows.ERROR_INVALID_NAME
		}
	}
	return list, nil
}

func (f *ftpWindowsFile) Stat() (os.FileInfo, error) {
	i, e := f.ftpFile.Stat()
	return i, ftpWindowsError(e)
}
func (f *ftpWindowsFile) Read(b []byte) (int, error) {
	n, e := f.ftpFile.Read(b)
	return n, ftpWindowsError(e)
}
func (f *ftpWindowsFile) ReadAt(b []byte, o int64) (int, error) {
	n, e := f.ftpFile.ReadAt(b, o)
	return n, ftpWindowsError(e)
}
func (f *ftpWindowsFile) Write(b []byte) (int, error) {
	n, e := f.ftpFile.Write(b)
	return n, ftpWindowsError(e)
}
func (f *ftpWindowsFile) WriteAt(b []byte, o int64) (int, error) {
	n, e := f.ftpFile.WriteAt(b, o)
	return n, ftpWindowsError(e)
}
func (f *ftpWindowsFile) Truncate(n int64) error { return ftpWindowsError(f.ftpFile.Truncate(n)) }
func (f *ftpWindowsFile) Sync() error            { return ftpWindowsError(f.ftpFile.Sync()) }
func (f *ftpWindowsFile) Close() error           { return ftpWindowsError(f.ftpFile.Close()) }

func isolateFTPCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
