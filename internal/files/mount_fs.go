package files

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"github.com/pkg/sftp/v2/encoding/ssh/filexfer/openssh"
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
	for p := range strings.SplitSeq(name, "/") {
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
	return f.statPath(p)
}

func (f *ftpFS) statPath(p string) (os.FileInfo, error) {
	v, err := f.loadMetadata(context.Background(), ftpCacheKey{kind: ftpStatKey, name: p}, false)
	return v.info, err
}

// SFTP sets access and modification times together. Fetch only the values
// the caller omitted, reusing any attributes already read for this change.
type ftpAttributeUpdate struct {
	size         *int64
	mode         *os.FileMode
	readOnly     *bool // Windows projects the read-only attribute onto permission bits.
	atime, mtime *time.Time
}

func ftpAccessTime(info os.FileInfo) time.Time {
	if attrs, ok := info.Sys().(*sshfx.Attributes); ok && attrs.HasACModTime() {
		return time.Unix(int64(attrs.ATime), 0)
	}
	return info.ModTime()
}

func (f *ftpFS) setAttributes(p string, opened *ftpFile, update ftpAttributeUpdate) error {
	paths := []string{p}
	var handle *sftp.File
	if opened != nil {
		handle = opened.handle
		if handle != nil && opened.path != p {
			paths = append(paths, opened.path)
		}
	}
	defer f.cache.beginMutation(paths...)()
	stat := func() (os.FileInfo, error) { return f.client.Stat(p) }
	if handle != nil {
		stat = handle.Stat
	}
	if update.size != nil {
		size := *update.size
		var err error
		switch {
		case handle != nil:
			err = handle.Truncate(size)
		case opened != nil:
			return syscall.EISDIR
		default:
			err = f.client.Truncate(p, size)
		}
		if err != nil {
			return err
		}
	}

	var info os.FileInfo
	mode := update.mode
	if update.readOnly != nil {
		var err error
		info, err = stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			perm := info.Mode().Perm()
			if *update.readOnly {
				perm &^= 0222
			} else {
				perm |= 0200
			}
			if perm != info.Mode().Perm() {
				mode = &perm
			}
		}
	}
	if mode != nil {
		var err error
		if handle != nil {
			err = handle.Chmod(*mode)
		} else {
			err = f.client.Chmod(p, *mode)
		}
		if err != nil {
			return err
		}
	}

	// SFTP v3 sets both timestamps in one request.
	atime, mtime := update.atime, update.mtime
	if atime == nil && mtime == nil {
		return nil
	}
	if (atime == nil || mtime == nil) && info == nil {
		var err error
		info, err = stat()
		if err != nil {
			return err
		}
	}
	if atime == nil {
		value := ftpAccessTime(info)
		atime = &value
	}
	if mtime == nil {
		value := info.ModTime()
		mtime = &value
	}
	if handle != nil {
		return handle.Chtimes(*atime, *mtime)
	}
	return f.client.Chtimes(p, *atime, *mtime)
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
	if e = f.client.Mkdir(p, mode.Perm()); e != nil {
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
	return f.client.RemoveFile(p)
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
	return f.client.Rename(a, b)
}
func (f *ftpFS) open(name string, flags int, mode os.FileMode) (*ftpFile, error) {
	return f.openWithMetadata(name, flags, mode, false)
}

func (f *ftpFS) openWithMetadata(name string, flags int, mode os.FileMode, cached bool) (*ftpFile, error) {
	writing := flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if writing {
		if e := f.writable(); e != nil {
			return nil, e
		}
	}
	p, e := f.remotePath(name)
	if e != nil {
		return nil, e
	}
	mutates := flags&(os.O_CREATE|os.O_TRUNC) != 0
	if mutates {
		defer f.cache.beginMutation(p)()
	}
	var info os.FileInfo
	// WinFsp queries path attributes before opening. Reuse that short-lived
	// result for classification, but always obtain a new remote file handle.
	if cached && !mutates {
		info, e = f.statPath(p)
	} else {
		info, e = f.client.Stat(p)
	}
	if e != nil && (!errors.Is(e, os.ErrNotExist) || flags&os.O_CREATE == 0) {
		return nil, e
	}
	if e == nil {
		if info.IsDir() && !writing {
			return &ftpFile{fs: f, path: p}, nil
		}
		if err := mountedRegularFile(info); err != nil {
			return nil, err
		}
	}
	file, e := f.openRegular(p, flags, mode, e == nil)
	if e != nil {
		return nil, e
	}
	return &ftpFile{handle: file, fs: f, path: p}, nil
}

func mountedRegularFile(info os.FileInfo) error {
	if info.IsDir() {
		return syscall.EISDIR
	}
	if !info.Mode().IsRegular() {
		return syscall.ENOTSUP
	}
	return nil
}

func (f *ftpFS) openRegular(p string, flags int, mode os.FileMode, exists bool) (*sftp.File, error) {
	if flags&os.O_CREATE == 0 {
		return f.client.OpenFile(p, flags, mode.Perm())
	}
	// Create exclusively before applying the locally requested mode. The
	// host umask can mask bits, and an existing file keeps its permissions.
	exclusive := flags&os.O_EXCL != 0
	existingFlags := flags &^ os.O_CREATE
	if exists && !exclusive {
		file, err := f.client.OpenFile(p, existingFlags, mode.Perm())
		if !errors.Is(err, os.ErrNotExist) {
			return file, err
		}
	}
	file, err := f.client.OpenFile(p, flags|os.O_EXCL, mode.Perm())
	if err != nil {
		// SFTP v3 servers can report an existing target as a generic failure.
		mayExist := errors.Is(err, os.ErrExist) || errors.Is(err, sshfx.StatusFailure)
		if exclusive || !mayExist {
			return nil, err
		}
		info, statErr := f.client.Stat(p)
		if statErr != nil {
			return nil, err
		}
		if typeErr := mountedRegularFile(info); typeErr != nil {
			return nil, typeErr
		}
		return f.client.OpenFile(p, existingFlags, mode.Perm())
	}
	if err = file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

type ftpFile struct {
	handle *sftp.File // nil for path-based directory handles
	fs     *ftpFS
	path   string
	mu     sync.Mutex
	dir    *ftpDirectory
	closed bool
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
	f.dir = nil
	if f.handle == nil {
		return nil
	}
	return f.handle.Close()
}
func (f *ftpFile) Stat() (os.FileInfo, error) {
	if f.handle == nil {
		return f.fs.client.Stat(f.path)
	}
	return f.handle.Stat()
}

// Handle-scoped keys preserve the identity of an open file across rename or
// replacement. Windows polls these handles independently of path metadata.
func (f *ftpFile) statCacheKey() ftpCacheKey {
	return ftpCacheKey{kind: ftpHandleKey, name: path.Dir(f.path), file: f}
}
func (f *ftpFile) cachedStat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	// SFTP directory handles here are path-based snapshots. WinFsp reopens
	// them for enumeration, so use ordinary path metadata and its query.
	if f.handle == nil {
		return f.fs.statPath(f.path)
	}
	v, err := f.fs.loadMetadata(context.Background(), f.statCacheKey(), false)
	return v.info, err
}

func (f *ftpFile) Readdir(n int) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	if f.handle != nil {
		return nil, syscall.ENOTDIR
	}
	if f.dir == nil {
		dir, e := f.fs.openDirectory(f.path)
		if e != nil {
			return nil, e
		}
		f.dir = &dir
	}
	if n > 0 && !f.dir.hasNext() {
		return nil, io.EOF
	}
	return f.dir.take(n), nil
}
func (f *ftpFile) Read(b []byte) (int, error) {
	if f.handle == nil {
		return 0, syscall.EISDIR
	}
	return f.handle.Read(b)
}
func (f *ftpFile) ReadAt(b []byte, o int64) (int, error) {
	if f.handle == nil {
		return 0, syscall.EISDIR
	}
	return f.handle.ReadAt(b, o)
}
func (f *ftpFile) Write(b []byte) (int, error) {
	if e := f.fs.writable(); e != nil {
		return 0, e
	}
	if f.handle == nil {
		return 0, syscall.EISDIR
	}
	defer f.fs.cache.beginMutation(f.path)()
	return f.handle.Write(b)
}
func (f *ftpFile) WriteAt(b []byte, o int64) (int, error) {
	if e := f.fs.writable(); e != nil {
		return 0, e
	}
	if f.handle == nil {
		return 0, syscall.EISDIR
	}
	defer f.fs.cache.beginMutation(f.path)()
	return f.handle.WriteAt(b, o)
}
func (f *ftpFile) Seek(o int64, w int) (int64, error) {
	if f.handle == nil {
		return 0, syscall.EISDIR
	}
	return f.handle.Seek(o, w)
}
func (f *ftpFile) Truncate(n int64) error {
	if e := f.fs.writable(); e != nil {
		return e
	}
	if f.handle == nil {
		return syscall.EISDIR
	}
	return f.fs.setAttributes(f.path, f, ftpAttributeUpdate{size: &n})
}
func (f *ftpFile) Sync() error {
	if f.handle == nil || f.fs.readOnly {
		return nil
	}
	return f.handle.Sync()
}

func ftpErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	if errors.Is(err, sshfx.StatusOpUnsupported) {
		return syscall.ENOTSUP
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno
	}
	if errors.Is(err, os.ErrNotExist) {
		return syscall.ENOENT
	}
	if errors.Is(err, os.ErrPermission) {
		return syscall.EACCES
	}
	if errors.Is(err, os.ErrExist) {
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

// Each open listing has its own cursor over an immutable cached snapshot.
type ftpDirectory struct {
	entries []os.FileInfo
	offset  int
}

func (d *ftpDirectory) hasNext() bool { return d.offset < len(d.entries) }

func (d *ftpDirectory) take(n int) []os.FileInfo {
	end := len(d.entries)
	if n > 0 && n < end-d.offset {
		end = d.offset + n
	}
	entries := d.entries[d.offset:end]
	d.offset = end
	return entries
}

func (f *ftpFS) openDirectory(p string) (ftpDirectory, error) {
	entries, err := f.readDirectory(p)
	if err != nil {
		return ftpDirectory{}, err
	}
	return ftpDirectory{entries: entries}, nil
}

func (f *ftpFS) readDirectory(p string) ([]os.FileInfo, error) {
	entries, err := f.loadDirectoryMode(context.Background(), p, false)
	// A foreground reader may have joined a speculative fetch that exhausted
	// its shorter deadline. Retry this read with the normal request deadline.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		entries, err = f.loadDirectoryMode(context.Background(), p, false)
	}
	if err == nil {
		f.prefetch.schedule(p, entries)
	}
	return entries, err
}
func (f *ftpFS) loadDirectoryMode(ctx context.Context, p string, prefetch bool) ([]os.FileInfo, error) {
	v, err := f.loadMetadata(ctx, ftpCacheKey{kind: ftpDirectoryKey, name: p}, prefetch)
	return v.entries, err
}

type DirectoryReader func(context.Context, string) ([]fs.DirEntry, error)

// NewSFTPDirectoryReader shares the mount directory cache policy within one
// authenticated SFTP session.
func NewSFTPDirectoryReader(client *sftp.Client) DirectoryReader {
	return (&ftpFS{client: client}).browseDirectory
}

func (f *ftpFS) browseDirectory(ctx context.Context, name string) ([]fs.DirEntry, error) {
	v, err := f.cache.loadMode(ctx, ftpCacheKey{kind: ftpDirectoryKey, name: name}, false, f.fetchMetadata)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, len(v.entries))
	for i, info := range v.entries {
		entries[i] = fs.FileInfoToDirEntry(info)
	}
	return entries, nil
}
func (f *ftpFS) lstat(p string) (os.FileInfo, error) {
	v, err := f.loadMetadata(context.Background(), ftpCacheKey{kind: ftpLstatKey, name: p}, false)
	return v.info, err
}

type ftpCapacityClient interface {
	HasExtension(*sshfx.ExtensionPair) bool
	StatVFS(string) (*openssh.StatVFSExtendedReplyPacket, error)
}

// Capacity is advisory; local mutations invalidate it and remote changes age out.
func (f *ftpFS) cachedCapacity(p string) (*openssh.StatVFSExtendedReplyPacket, error) {
	v, err := f.loadMetadata(context.Background(), ftpCacheKey{kind: ftpCapacityKey, name: p}, false)
	if err != nil {
		return nil, err
	}
	return v.capacity, v.capacityErr
}

func (f *ftpFS) loadMetadata(ctx context.Context, key ftpCacheKey, speculative bool) (ftpCacheValue, error) {
	if !speculative {
		dir := key.parentDirectory()
		if key.kind == ftpDirectoryKey {
			dir = key.name
		}
		f.notifications.enqueue(dir)
	}
	return f.cache.loadMode(ctx, key, speculative, f.fetchMetadata)
}

// Foreground and background path queries use the same metadata source.
func (f *ftpFS) fetchMetadata(ctx context.Context, key ftpCacheKey) (ftpCacheValue, error) {
	name := key.name
	switch key.kind {
	case ftpStatKey:
		info, err := f.client.Stat(name)
		return statCacheValue(name, info), err
	case ftpLstatKey:
		info, err := f.client.LStat(name)
		return statCacheValue(name, info), err
	case ftpDirectoryKey:
		dir, err := f.client.OpenDir(name)
		if err != nil {
			return ftpCacheValue{}, err
		}
		defer dir.Close()
		entries, err := dir.ReaddirContext(ctx, 0)
		if err != nil {
			return ftpCacheValue{}, err
		}
		if len(entries) > ftpMaxDirectoryEntries {
			return ftpCacheValue{}, syscall.EOVERFLOW
		}
		return directoryValue(name, entries), nil
	case ftpCapacityKey:
		capacity, err := ftpRemoteCapacity(f.client, name)
		// Bound failed advisory probes too; callers receive the original error.
		return ftpCacheValue{capacity: capacity, capacityErr: err, bytes: 256 + len(name)}, nil
	case ftpHandleKey:
		info, err := key.file.Stat()
		return ftpCacheValue{info: info, bytes: 256 + ftpInfoBytes(info)}, err
	default:
		return ftpCacheValue{}, os.ErrInvalid
	}
}

func ftpRemoteCapacity(client ftpCapacityClient, root string) (*openssh.StatVFSExtendedReplyPacket, error) {
	if !client.HasExtension(openssh.ExtensionStatVFS()) {
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

func validateFTPCapacity(s *openssh.StatVFSExtendedReplyPacket) error {
	// Validate before narrowing FUSE fields or converting block counts to bytes.
	if s == nil || s.FragmentSize == 0 || s.BlockSize == 0 ||
		s.FragmentSize > math.MaxUint32 || s.BlockSize > math.MaxUint32 || s.MaxNameLength > math.MaxUint32 ||
		s.Blocks > math.MaxUint64/s.FragmentSize || s.BlocksFree > s.Blocks || s.BlocksAvail > s.BlocksFree {
		return syscall.EIO
	}
	return nil
}
