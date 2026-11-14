package main

// The mount backend uses SFTP handles directly: writes wait for server replies,
// with no local write-back spool or automatic replay. Remote paths use POSIX
// separators on every client OS. The selected root is a navigation root, not a
// remote security sandbox: symlinks and the authenticated account's permissions
// determine remote access. Directory handles hold one bounded snapshot; reopening
// refreshes it. Native adapters own OS-specific names, attributes and errors.

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/pkg/sftp"
)

const ftpMaxDirectoryEntries = 100000

type ftpFS struct {
	client   *sftp.Client
	root     string
	readOnly bool
	directIO bool
}

func (f *ftpFS) remotePath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", os.ErrInvalid
	}
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return "", os.ErrPermission
		}
	}
	return path.Join(f.root, path.Clean("/"+name)), nil
}
func (f *ftpFS) writable() error {
	if f.readOnly {
		return syscall.EROFS
	}
	return nil
}
func (f *ftpFS) stat(name string) (os.FileInfo, error) {
	p, e := f.remotePath(name)
	if e != nil {
		return nil, e
	}
	return f.client.Stat(p)
}
func (f *ftpFS) mkdir(name string, mode os.FileMode) error {
	if e := f.writable(); e != nil {
		return e
	}
	p, e := f.remotePath(name)
	if e != nil {
		return e
	}
	if e = f.client.Mkdir(p); e != nil {
		return e
	}
	return f.client.Chmod(p, mode.Perm())
}
func (f *ftpFS) remove(name string, dir bool) error {
	if e := f.writable(); e != nil {
		return e
	}
	p, e := f.remotePath(name)
	if e != nil {
		return e
	}
	if p == f.root {
		return os.ErrPermission
	}
	if dir {
		return f.client.RemoveDirectory(p)
	}
	return f.client.Remove(p)
}
func (f *ftpFS) rename(old, new string) error {
	if e := f.writable(); e != nil {
		return e
	}
	a, e := f.remotePath(old)
	if e != nil {
		return e
	}
	b, e := f.remotePath(new)
	if e != nil {
		return e
	}
	if a == f.root || b == f.root {
		return os.ErrPermission
	}
	// Require the atomic overwrite extension at connection setup for writable
	// mounts. Never emulate replacement by deleting the destination first.
	return f.client.PosixRename(a, b)
}
func (f *ftpFS) open(name string, flags int, mode os.FileMode) (*ftpFile, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		if e := f.writable(); e != nil {
			return nil, e
		}
	}
	p, e := f.remotePath(name)
	if e != nil {
		return nil, e
	}
	info, e := f.client.Stat(p)
	if e == nil && info.IsDir() {
		if flags&os.O_CREATE != 0 {
			return nil, syscall.EISDIR
		}
		if flags&(os.O_WRONLY|os.O_RDWR|os.O_TRUNC|os.O_APPEND) != 0 {
			return nil, syscall.EISDIR
		}
		return &ftpFile{fs: f, path: p, dir: true}, nil
	}
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if e == nil && !info.Mode().IsRegular() {
		return nil, syscall.ENOTSUP
	}
	// SFTP's public OpenFile API cannot attach creation permissions. Create
	// exclusively, then chmod the empty file before returning its handle. Never
	// chmod an existing destination or retry a failed write. If another creator
	// wins, open the existing file without O_CREATE so disappearance fails closed.
	var file *sftp.File
	if flags&os.O_CREATE != 0 {
		file, e = f.client.OpenFile(p, flags|os.O_EXCL)
		if e == nil {
			if e = file.Chmod(mode.Perm()); e != nil {
				_ = file.Close()
				return nil, e
			}
		} else if flags&os.O_EXCL == 0 {
			if _, statErr := f.client.Stat(p); statErr != nil {
				return nil, e
			}
			file, e = f.client.OpenFile(p, flags&^os.O_CREATE)
		}
	} else {
		file, e = f.client.OpenFile(p, flags)
	}
	if e != nil {
		return nil, e
	}

	return &ftpFile{File: file, fs: f, path: p}, nil
}

type ftpFile struct {
	*sftp.File
	fs             *ftpFS
	path           string
	dir            bool
	mu             sync.Mutex
	entries        []os.FileInfo
	loaded, closed bool
	offset         int
}

func (f *ftpFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.entries = nil
	if f.dir {
		return nil
	}
	return f.File.Close()
}
func (f *ftpFile) Stat() (os.FileInfo, error) {
	if f.dir {
		return f.fs.client.Stat(f.path)
	}
	return f.File.Stat()
}
func (f *ftpFile) Readdir(n int) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	if !f.dir {
		return nil, syscall.ENOTDIR
	}
	if !f.loaded {
		// The framing guard enforces entry/byte budgets before decoding. This
		// count check also protects callers supplying an unguarded test client.
		list, e := f.fs.client.ReadDir(f.path)
		if e != nil {
			return nil, e
		}
		if len(list) > ftpMaxDirectoryEntries {
			return nil, syscall.EOVERFLOW
		}
		f.entries = list
		f.loaded = true
	}
	if n > 0 && f.offset == len(f.entries) {
		return nil, io.EOF
	}
	end := len(f.entries)
	if n > 0 && n < end-f.offset {
		end = f.offset + n
	}
	list := f.entries[f.offset:end]
	f.offset = end
	return list, nil
}
func (f *ftpFile) Read(b []byte) (int, error) {
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.Read(b)
}
func (f *ftpFile) ReadAt(b []byte, o int64) (int, error) {
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.ReadAt(b, o)
}
func (f *ftpFile) Write(b []byte) (int, error) {
	if e := f.fs.writable(); e != nil {
		return 0, e
	}
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.Write(b)
}
func (f *ftpFile) WriteAt(b []byte, o int64) (int, error) {
	if e := f.fs.writable(); e != nil {
		return 0, e
	}
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.WriteAt(b, o)
}
func (f *ftpFile) Seek(o int64, w int) (int64, error) {
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.Seek(o, w)
}
func (f *ftpFile) Truncate(n int64) error {
	if e := f.fs.writable(); e != nil {
		return e
	}
	if f.dir {
		return syscall.EISDIR
	}
	return f.File.Truncate(n)
}
func (f *ftpFile) Sync() error {
	if f.dir || f.fs.readOnly {
		return nil
	}
	return f.File.Sync()
}

func ftpErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	var status *sftp.StatusError
	if errors.As(err, &status) {
		switch status.FxCode() {
		case sftp.ErrSSHFxNoSuchFile:
			return syscall.ENOENT
		case sftp.ErrSSHFxPermissionDenied:
			return syscall.EACCES
		case sftp.ErrSSHFxOpUnsupported:
			return syscall.ENOTSUP
		default:
			return syscall.EIO
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	if os.IsNotExist(err) {
		return syscall.ENOENT
	}
	if os.IsPermission(err) {
		return syscall.EACCES
	}
	if os.IsExist(err) {
		return syscall.EEXIST
	}
	if errors.Is(err, os.ErrInvalid) {
		return syscall.EINVAL
	}
	if errors.Is(err, os.ErrClosed) {
		return syscall.EBADF
	}
	return syscall.EIO
}
