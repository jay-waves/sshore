//go:build windows

package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

// WinFsp uses its native Go binding, without CGO. gofs manages Windows handle
// sharing and rename bookkeeping; the adapter translates remote errors and
// rejects names that Windows cannot represent rather than aliasing them.

func ValidateMountpoint(p string) error {
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

type ftpWindowsFS struct {
	backend *ftpFS
}

func ftpWindowsPath(p string) (string, error) {
	p = strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/")
	for _, part := range strings.Split(p, "/") {
		if part == "" {
			continue
		}
		if e := ValidateName(part); e != nil {
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
	h, e := f.backend.openWithMetadata(p, flags, perm, true)
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

type ftpWindowsFile struct {
	*ftpFile
}

func (f *ftpWindowsFile) Readdir(n int) ([]os.FileInfo, error) {
	list, e := f.ftpFile.Readdir(n)
	if e != nil {
		return nil, ftpWindowsError(e)
	}
	for _, i := range list {
		if e := ValidateName(i.Name()); e != nil {
			return nil, windows.ERROR_INVALID_NAME
		}
	}
	return list, nil
}

func (f *ftpWindowsFile) Stat() (os.FileInfo, error) {
	i, e := f.ftpFile.cachedStat()
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
func (f *ftpWindowsFile) Truncate(n int64) error {
	return ftpWindowsError(f.ftpFile.Truncate(n))
}
func (f *ftpWindowsFile) Sync() error  { return ftpWindowsError(f.ftpFile.Sync()) }
func (f *ftpWindowsFile) Close() error { return ftpWindowsError(f.ftpFile.Close()) }

const winFspInstallURL = "https://winfsp.dev/"

var winFspCheckOnce sync.Once
var winFspCheckError error

func CheckMountPlatform() error {
	winFspCheckOnce.Do(func() { winFspCheckError = checkWinFsp(winfsp.BinPath, winfsp.LoadWinFSP) })
	return winFspCheckError
}
func checkWinFsp(find func() (string, error), load func() error) error {
	if _, e := find(); e != nil {
		if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) || errors.Is(e, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("WinFsp was not found. Download and install it from %s, then retry: %w", winFspInstallURL, e)
		}
		return fmt.Errorf("cannot inspect the WinFsp installation: %w", e)
	}
	if e := load(); e != nil {
		return fmt.Errorf("WinFsp is installed but could not be loaded; repair the installation from %s, then retry: %w", winFspInstallURL, e)
	}
	return nil
}

// gofs supplies handle sharing, directory buffers and native rename bookkeeping,
// but its default SetBasicInfo rejects every request. This wrapper adds SFTP mtime,
// atime and read-only mode updates. DOS archive/hidden/system flags and creation/
// change times are not stored by SFTP v3; they remain synthetic, as with many
// network mounts. Path tracking is serialized with local rename/close callbacks;
// concurrent remote changes remain subject to ordinary SFTP path semantics.

type ftpWindowsBehaviour interface {
	winfsp.BehaviourBase
	winfsp.BehaviourGetSecurityByName
	winfsp.BehaviourCreate
	winfsp.BehaviourOverwrite
	winfsp.BehaviourReadDirectory
	winfsp.BehaviourGetFileInfo
	winfsp.BehaviourGetSecurity
	winfsp.BehaviourGetVolumeInfo
	winfsp.BehaviourSetVolumeLabel
	winfsp.BehaviourSetFileSize
	winfsp.BehaviourRead
	winfsp.BehaviourWrite
	winfsp.BehaviourFlush
	winfsp.BehaviourCanDelete
	winfsp.BehaviourCleanup
	winfsp.BehaviourRename
	winfsp.BehaviourDefaultOptions
}

type ftpWindowsAttributes struct {
	ftpWindowsBehaviour
	backend *ftpFS
	// Exclusive callbacks wait for in-flight opens before changing tracked paths.
	mu      sync.RWMutex
	namesMu sync.Mutex
	names   map[uintptr]string
}

func (a *ftpWindowsAttributes) Open(ref *winfsp.FileSystemRef, name string, opts, access uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	defer a.backend.metrics.observe("Open")()
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, e := a.ftpWindowsBehaviour.Open(ref, name, opts, access, info)
	if e == nil {
		a.namesMu.Lock()
		a.names[h] = name
		a.namesMu.Unlock()
	}
	return h, e
}
func (a *ftpWindowsAttributes) Create(ref *winfsp.FileSystemRef, name string, opts, access, attrs uint32, sd *windows.SECURITY_DESCRIPTOR, size uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	defer a.backend.metrics.observe("Create")()
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, e := a.ftpWindowsBehaviour.Create(ref, name, opts, access, attrs, sd, size, info)
	if e == nil {
		a.namesMu.Lock()
		a.names[h] = name
		a.namesMu.Unlock()
	}
	return h, e
}
func (a *ftpWindowsAttributes) Close(ref *winfsp.FileSystemRef, h uintptr) {
	defer a.backend.metrics.observe("Close")()
	a.mu.Lock()
	delete(a.names, h)
	a.mu.Unlock()
	a.ftpWindowsBehaviour.Close(ref, h)
}
func (a *ftpWindowsAttributes) Rename(ref *winfsp.FileSystemRef, h uintptr, old, new string, replace bool) error {
	defer a.backend.metrics.observe("Rename")()
	a.mu.Lock()
	defer a.mu.Unlock()
	source, ok := a.names[h]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}
	if e := a.ftpWindowsBehaviour.Rename(ref, h, old, new, replace); e != nil {
		return e
	}
	// Handles to an overwritten target must not apply path-based attributes to
	// the replacement. Their existing data handles remain under gofs ownership.
	if source != new {
		for handle, name := range a.names {
			if name == new {
				delete(a.names, handle)
			}
		}
	}
	for handle, name := range a.names {
		if name == source {
			a.names[handle] = new
		} else if strings.HasPrefix(name, source+`\`) {
			a.names[handle] = new + strings.TrimPrefix(name, source)
		}
	}
	return nil
}
func ftpWindowsTime(v uint64) time.Time {
	t := windows.Filetime{LowDateTime: uint32(v), HighDateTime: uint32(v >> 32)}
	return time.Unix(0, t.Nanoseconds())
}
func (a *ftpWindowsAttributes) SetBasicInfo(ref *winfsp.FileSystemRef, h uintptr, flags winfsp.SetBasicInfoFlags, attrs uint32, creation, access, write, change uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	defer a.backend.metrics.observe("SetBasicInfo")()
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.backend.writable(); e != nil {
		return ftpWindowsError(e)
	}
	name, ok := a.names[h]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}
	// Verify the underlying gofs handle before using its tracked pathname.
	if e := a.GetFileInfo(ref, h, info); e != nil {
		return e
	}
	if flags&(winfsp.SetBasicInfoAttributes|winfsp.SetBasicInfoLastAccessTime|winfsp.SetBasicInfoLastWriteTime) == 0 {
		return nil
	}
	name, e := ftpWindowsPath(name)
	if e != nil {
		return e
	}
	p, e := a.backend.remotePath(name)
	if e != nil {
		return ftpWindowsError(e)
	}
	defer a.backend.cache.beginMutation(p)()
	stat, e := a.backend.client.Stat(p)
	if e != nil {
		return ftpWindowsError(e)
	}
	if flags&winfsp.SetBasicInfoAttributes != 0 && !stat.IsDir() {
		mode := stat.Mode().Perm()
		if attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			mode &^= 0222
		} else {
			mode |= 0200
		}
		if mode != stat.Mode().Perm() {
			if e = a.backend.client.Chmod(p, os.FileMode(mode)); e != nil {
				return ftpWindowsError(e)
			}
		}
	}
	if flags&(winfsp.SetBasicInfoLastAccessTime|winfsp.SetBasicInfoLastWriteTime) != 0 {
		at, mt := stat.ModTime(), stat.ModTime()
		if raw, ok := stat.Sys().(*sftp.FileStat); ok {
			at = time.Unix(int64(raw.Atime), 0)
		}
		if flags&winfsp.SetBasicInfoLastAccessTime != 0 {
			at = ftpWindowsTime(access)
		}
		if flags&winfsp.SetBasicInfoLastWriteTime != 0 {
			mt = ftpWindowsTime(write)
		}
		if e = a.backend.client.Chtimes(p, at, mt); e != nil {
			return ftpWindowsError(e)
		}
	}
	return a.GetFileInfo(ref, h, info)
}

func (a *ftpWindowsAttributes) ReadDirectory(ref *winfsp.FileSystemRef, h uintptr, pattern string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	defer a.backend.metrics.observe("ReadDirectory")()
	return a.ftpWindowsBehaviour.ReadDirectory(ref, h, pattern, fill)
}
func (a *ftpWindowsAttributes) GetFileInfo(ref *winfsp.FileSystemRef, h uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	defer a.backend.metrics.observe("GetFileInfo")()
	return a.ftpWindowsBehaviour.GetFileInfo(ref, h, info)
}

func (a *ftpWindowsAttributes) GetVolumeInfo(ref *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	s, err := ftpRemoteCapacity(a.backend.client, a.backend.root)
	if err != nil {
		return ftpWindowsError(err)
	}
	if err := a.ftpWindowsBehaviour.GetVolumeInfo(ref, info); err != nil {
		return err
	}
	info.TotalSize = s.Blocks * s.Frsize
	info.FreeSize = s.Bavail * s.Frsize
	return nil
}

func (a *ftpWindowsAttributes) SetVolumeLabel(ref *winfsp.FileSystemRef, label string, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	if err := a.ftpWindowsBehaviour.SetVolumeLabel(ref, label, info); err != nil {
		return err
	}
	return a.GetVolumeInfo(ref, info)
}

func openSourceFile(path string) (*os.File, error) {
	// Use an extended absolute path so native CreateFile retains os.Open's long
	// path support rather than imposing a new MAX_PATH restriction.
	native, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(native, `\\?\`) && !strings.HasPrefix(native, `\\.\`) {
		if strings.HasPrefix(native, `\\`) {
			native = `\\?\UNC\` + native[2:]
		} else {
			native = `\\?\` + native
		}
	}
	name, err := windows.UTF16PtrFromString(native)
	if err != nil {
		return nil, err
	}
	// Inspect the reparse point itself, rather than following a replacement link.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
