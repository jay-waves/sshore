package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/pkg/sftp"
)

// The mount backend uses SFTP handles directly: writes wait for server replies,
// with no local write-back spool or automatic replay. Remote paths use POSIX
// separators on every client OS. The selected root is a navigation root, not a
// remote security sandbox: symlinks and the authenticated account's permissions
// determine remote access. Directory handles hold one bounded snapshot; reopening
// may reuse a short-lived mount cache. Native adapters own OS-specific names, attributes and errors.

const ftpMaxDirectoryEntries = 100000

type ftpFS struct {
	client        *sftp.Client
	root          string
	readOnly      bool
	directIO      bool
	cache         ftpMetadataCache
	metrics       *ftpCallbackMetrics
	notifications *ftpNotifications
	prefetch      *directoryPrefetch
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
	f.watchDirectory(path.Dir(p))
	return f.cache.stat(p, func() (os.FileInfo, error) { return f.client.Stat(p) })
}
func (f *ftpFS) mkdir(name string, mode os.FileMode) error {
	if e := f.writable(); e != nil {
		return e
	}
	p, e := f.remotePath(name)
	if e != nil {
		return e
	}
	defer f.cache.beginMutation(p)()
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
	defer f.cache.beginMutation(p)()
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
	defer f.cache.beginMutation(a, b)()
	// Require the atomic overwrite extension at connection setup for writable
	// mounts. Never emulate replacement by deleting the destination first.
	return f.client.PosixRename(a, b)
}
func (f *ftpFS) open(name string, flags int, mode os.FileMode) (*ftpFile, error) {
	return f.openWithMetadata(name, flags, mode, false)
}

func (f *ftpFS) openWithMetadata(name string, flags int, mode os.FileMode, cached bool) (*ftpFile, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		if e := f.writable(); e != nil {
			return nil, e
		}
	}
	p, e := f.remotePath(name)
	if e != nil {
		return nil, e
	}
	if flags&(os.O_CREATE|os.O_TRUNC) != 0 {
		defer f.cache.beginMutation(p)()
	}
	var info os.FileInfo
	// WinFsp queries path attributes before opening. Reuse that short-lived
	// result for classification, but always obtain a new remote file handle.
	if cached && flags&(os.O_CREATE|os.O_TRUNC) == 0 {
		info, e = f.stat(name)
	} else {
		info, e = f.client.Stat(p)
	}
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
	f.fs.cache.mu.Lock()
	if e := f.fs.cache.entries[f.statCacheKey()]; e != nil {
		f.fs.cache.removeLocked(e)
	}
	f.fs.cache.mu.Unlock()
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

// Handle-scoped keys preserve the identity of an open file across rename or
// replacement. Windows polls these handles independently of path metadata.
func (f *ftpFile) statCacheKey() string { return fmt.Sprintf("h:%p", f) }
func (f *ftpFile) cachedStat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	f.fs.watchDirectory(path.Dir(f.path))
	// SFTP directory handles here are path-based snapshots, not open remote
	// files. WinFsp reopens them for enumeration, so share path metadata.
	if f.dir {
		return f.fs.cache.stat(f.path, func() (os.FileInfo, error) { return f.Stat() })
	}
	v, err := f.fs.cache.loadScoped(f.statCacheKey(), path.Dir(f.path), ftpMetadataTTL, func() (ftpCacheValue, error) {
		info, err := f.Stat()
		return ftpCacheValue{info: info, bytes: 256 + ftpInfoBytes(info)}, err
	})
	return v.info, err
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
		list, e := f.fs.readDirectory(f.path)
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
	defer f.fs.cache.beginMutation(f.path)()
	if e := f.fs.writable(); e != nil {
		return 0, e
	}
	if f.dir {
		return 0, syscall.EISDIR
	}
	return f.File.Write(b)
}
func (f *ftpFile) WriteAt(b []byte, o int64) (int, error) {
	defer f.fs.cache.beginMutation(f.path)()
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
	defer f.fs.cache.beginMutation(f.path)()
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

func (f *ftpFS) readDirectory(p string) ([]os.FileInfo, error) {
	f.watchDirectory(p)
	entries, err := f.loadDirectory(context.Background(), p)
	// A foreground reader may have joined a speculative fetch that exhausted
	// its shorter deadline. Retry this read with the normal request deadline.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		entries, err = f.loadDirectory(context.Background(), p)
	}
	if err == nil {
		f.prefetch.schedule(p, entries)
	}
	return entries, err
}
func (f *ftpFS) loadDirectory(ctx context.Context, p string) ([]os.FileInfo, error) {
	return f.loadDirectoryMode(ctx, p, false)
}
func (f *ftpFS) loadDirectoryMode(ctx context.Context, p string, prefetch bool) ([]os.FileInfo, error) {
	return f.cache.readDirMode(p, prefetch, func() ([]os.FileInfo, error) {
		entries, err := f.client.ReadDirContext(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(entries) > ftpMaxDirectoryEntries {
			return nil, syscall.EOVERFLOW
		}
		return entries, nil
	})
}
func (f *ftpFS) lstat(p string) (os.FileInfo, error) {
	f.watchDirectory(path.Dir(p))
	v, err := f.cache.load("l:"+p, ftpMetadataTTL, func() (ftpCacheValue, error) {
		info, err := f.client.Lstat(p)
		return ftpCacheValue{info: info, bytes: 256 + len(p) + ftpInfoBytes(info)}, err
	})
	return v.info, err
}

type ftpCapacityClient interface {
	HasExtension(string) (string, bool)
	StatVFS(string) (*sftp.StatVFS, error)
}

func ftpRemoteCapacity(client ftpCapacityClient, root string) (*sftp.StatVFS, error) {
	version, ok := client.HasExtension("statvfs@openssh.com")
	if !ok || version != "2" {
		return nil, syscall.ENOTSUP
	}
	stat, err := client.StatVFS(root)
	if err != nil {
		return nil, err
	}
	if err := validateFTPCapacity(stat); err != nil {
		return nil, err
	}
	return stat, nil
}

func validateFTPCapacity(s *sftp.StatVFS) error {
	// Validate before narrowing FUSE fields or converting block counts to bytes.
	if s == nil || s.Frsize == 0 || s.Bsize == 0 ||
		s.Frsize > math.MaxUint32 || s.Bsize > math.MaxUint32 || s.Namemax > math.MaxUint32 ||
		s.Blocks > math.MaxUint64/s.Frsize || s.Bfree > s.Blocks || s.Bavail > s.Bfree {
		return syscall.EIO
	}
	return nil
}
