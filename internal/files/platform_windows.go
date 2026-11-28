//go:build windows

package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

// WinFsp uses its native Go binding, without CGO. gofs manages Windows handle
// sharing and rename bookkeeping; the adapter translates remote errors and
// rejects names that Windows cannot represent rather than aliasing them.

func ValidateMountpoint(p string) error {
	if isDriveLetter(p) {
		drives, err := windows.GetLogicalDrives()
		if err != nil {
			return err
		}
		if drives&driveBit(p) != 0 {
			return fmt.Errorf("drive %s is already in use", p)
		}
		return nil
	}
	if _, e := os.Lstat(p); !os.IsNotExist(e) {
		return fmt.Errorf("Windows mountpoint must be an unused drive letter or a nonexistent directory")
	}
	return nil
}

func isDriveLetter(p string) bool {
	return len(p) == 2 && p[1] == ':' && (p[0]&^0x20) >= 'A' && (p[0]&^0x20) <= 'Z'
}

func driveBit(p string) uint32 { return 1 << ((p[0] &^ 0x20) - 'A') }

type ftpWindowsMount struct {
	fs      *winfsp.FileSystem
	done    chan struct{}
	once    sync.Once
	cleanup func()
}

func (m *ftpWindowsMount) Wait() { <-m.done }
func (m *ftpWindowsMount) Unmount() error {
	m.once.Do(func() { m.fs.Unmount(); m.cleanup(); close(m.done) })
	return nil
}
func mountFTP(f *ftpFS, p string, automatic bool) (ftpMount, string, error) {
	opts := []winfsp.Option{winfsp.FileSystemName("sshore")}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, "", fmt.Errorf("identify Windows mount owner: %w", err)
	}
	group, err := windows.GetCurrentProcessToken().GetTokenPrimaryGroup()
	if err != nil {
		return nil, "", fmt.Errorf("identify Windows mount group: %w", err)
	}
	sid := user.User.Sid.String()
	// The UNC path can outlive a logon session's drive-letter namespace.
	// Give the mounting account and SYSTEM access through either spelling.
	sddl := "O:" + sid + "G:" + group.PrimaryGroup.String() + "D:P(A;;FA;;;" + sid + ")(A;;FA;;;SY)"
	security, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, "", fmt.Errorf("create Windows mount permissions: %w", err)
	}
	if f.readOnly {
		opts = append(opts, winfsp.Attributes(winfsp.FspFSAttributeReadOnlyVolume))
	}
	name, number := "", 1
	if isDriveLetter(p) {
		// Windows can resolve final paths on WinFsp network drives.
		name = windowsMountShareName(f.root)
		opts = append(opts, winfsp.VolumePrefix(`\sshore\`+name))
	}
	pool := &ftpWindowsMetadataPool{backend: f, entries: make(map[string]*ftpWindowsMetadataEntry)}
	behaviour := gofs.New(&ftpWindowsFS{backend: f, pool: pool}).(ftpWindowsBehaviour)
	adapter := &ftpWindowsAttributes{ftpWindowsBehaviour: behaviour, backend: f, security: security}
	var tried uint32
	for {
		host, err := winfsp.Mount(adapter, p, opts...)
		if err == nil {
			return &ftpWindowsMount{fs: host, done: make(chan struct{}), cleanup: pool.close}, p, nil
		}
		if errors.Is(err, winfsp.ErrVolumePrefixInUse) {
			number++
			opts[len(opts)-1] = winfsp.VolumePrefix(`\sshore\` + name + "-" + strconv.Itoa(number))
			continue
		}
		if !automatic || !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			pool.close()
			return nil, "", fmt.Errorf("WinFsp mount failed: %w", err)
		}
		// Another mount won this drive while this worker was connecting.
		tried |= driveBit(p)
		p, err = availableDrive(tried)
		if err != nil {
			pool.close()
			return nil, "", err
		}
	}
}

func windowsMountShareName(root string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`"\/[]:|<>+=;,*?`, r) {
			return '-'
		}
		// WinFsp resolves UNC paths case-insensitively.
		return unicode.ToLower(unicode.ToUpper(r))
	}, path.Base(root))
	// Leave room for the numeric suffix in the 80-code-unit share name.
	remaining := 60
	for i, r := range name {
		remaining -= utf16.RuneLen(r)
		if remaining < 0 {
			name = name[:i]
			break
		}
	}
	name = strings.Trim(name, " .-")
	if name == "" {
		return "root"
	}
	return name
}

type ftpWindowsFS struct {
	backend *ftpFS
	pool    *ftpWindowsMetadataPool
}

func ftpWindowsPath(p string) (string, error) {
	p = strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/")
	for part := range strings.SplitSeq(p, "/") {
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
	i, e := f.backend.client.LStat(remote)
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

var checkMountPlatform = sync.OnceValue(func() error {
	return winFspLoadError(winfsp.LoadWinFSP(), winfsp.BinPath)
})

func CheckMountPlatform() error { return checkMountPlatform() }
func winFspLoadError(loadErr error, find func() (string, error)) error {
	if loadErr == nil {
		return nil
	}
	// The loader already looked up the installation. Only repeat the lookup
	// on failure to distinguish a missing installation from a broken DLL.
	if _, e := find(); e != nil {
		if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) || errors.Is(e, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("WinFsp was not found. Download and install it from %s, then retry: %w", winFspInstallURL, e)
		}
		return fmt.Errorf("cannot inspect the WinFsp installation: %w", e)
	}
	return fmt.Errorf("WinFsp is installed but could not be loaded; repair the installation from %s, then retry: %w", winFspInstallURL, loadErr)
}

// gofs owns native handles and directory locks. The adapter records callback
// timings and reports the backing filesystem's capacity.
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
	winfsp.BehaviourSetBasicInfo
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
	backend  *ftpFS
	security *windows.SECURITY_DESCRIPTOR
}

func (a *ftpWindowsAttributes) GetSecurityByName(ref *winfsp.FileSystemRef, name string, flags winfsp.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	attributes, sd, err := a.ftpWindowsBehaviour.GetSecurityByName(ref, name, flags)
	if sd != nil {
		sd = a.security
	}
	return attributes, sd, err
}

func (a *ftpWindowsAttributes) GetSecurity(ref *winfsp.FileSystemRef, h uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	_, err := a.ftpWindowsBehaviour.GetSecurity(ref, h)
	return a.security, err
}

func (a *ftpWindowsAttributes) Open(ref *winfsp.FileSystemRef, name string, opts, access uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	defer a.backend.metrics.observe(ftpCallbackOpen)()
	return a.ftpWindowsBehaviour.Open(ref, name, opts, access, info)
}
func (a *ftpWindowsAttributes) Create(ref *winfsp.FileSystemRef, name string, opts, access, attrs uint32, sd *windows.SECURITY_DESCRIPTOR, size uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	defer a.backend.metrics.observe(ftpCallbackCreate)()
	return a.ftpWindowsBehaviour.Create(ref, name, opts, access, attrs, sd, size, info)
}
func (a *ftpWindowsAttributes) Close(ref *winfsp.FileSystemRef, h uintptr) {
	defer a.backend.metrics.observe(ftpCallbackClose)()
	a.ftpWindowsBehaviour.Close(ref, h)
}
func (a *ftpWindowsAttributes) Rename(ref *winfsp.FileSystemRef, h uintptr, old, new string, replace bool) error {
	defer a.backend.metrics.observe(ftpCallbackRename)()
	return a.ftpWindowsBehaviour.Rename(ref, h, old, new, replace)
}
func ftpWindowsTime(v uint64) time.Time {
	t := windows.Filetime{LowDateTime: uint32(v), HighDateTime: uint32(v >> 32)}
	return time.Unix(0, t.Nanoseconds())
}
func (a *ftpWindowsAttributes) SetBasicInfo(ref *winfsp.FileSystemRef, h uintptr, flags winfsp.SetBasicInfoFlags, attrs uint32, creation, access, write, change uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	defer a.backend.metrics.observe(ftpCallbackSetBasicInfo)()
	return a.ftpWindowsBehaviour.SetBasicInfo(ref, h, flags, attrs, creation, access, write, change, info)
}

// WinFsp supplies the current path under its rename lock. Regular files
// retain their SFTP handle even when another process replaces that path.
func (f *ftpWindowsFile) SetBasicInfo(name string, flags winfsp.SetBasicInfoFlags, attrs uint32, _, access, write, _ uint64) (os.FileInfo, error) {
	if err := f.fs.writable(); err != nil {
		return nil, ftpWindowsError(err)
	}
	p := f.path
	if name != "" {
		local, err := ftpWindowsPath(name)
		if err != nil {
			return nil, err
		}
		p, err = f.fs.remotePath(local)
		if err != nil {
			return nil, ftpWindowsError(err)
		}
	} else if f.handle == nil {
		return nil, windows.STATUS_INVALID_HANDLE
	}
	var update ftpAttributeUpdate
	if flags&winfsp.SetBasicInfoAttributes != 0 {
		readOnly := attrs&windows.FILE_ATTRIBUTE_READONLY != 0
		update.readOnly = &readOnly
	}
	if flags&winfsp.SetBasicInfoLastAccessTime != 0 {
		at := ftpWindowsTime(access)
		update.atime = &at
	}
	if flags&winfsp.SetBasicInfoLastWriteTime != 0 {
		mt := ftpWindowsTime(write)
		update.mtime = &mt
	}
	if update != (ftpAttributeUpdate{}) {
		if err := f.fs.setAttributes(p, f.ftpFile, update); err != nil {
			return nil, ftpWindowsError(err)
		}
	}
	if f.handle == nil {
		info, err := f.fs.statPath(p)
		return info, ftpWindowsError(err)
	}
	return f.Stat()
}

var _ gofs.FileSetBasicInfo = (*ftpWindowsFile)(nil)

func (a *ftpWindowsAttributes) ReadDirectory(ref *winfsp.FileSystemRef, h uintptr, pattern string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	defer a.backend.metrics.observe(ftpCallbackReadDirectory)()
	return a.ftpWindowsBehaviour.ReadDirectory(ref, h, pattern, fill)
}
func (a *ftpWindowsAttributes) GetFileInfo(ref *winfsp.FileSystemRef, h uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	defer a.backend.metrics.observe(ftpCallbackGetFileInfo)()
	return a.ftpWindowsBehaviour.GetFileInfo(ref, h, info)
}

func (a *ftpWindowsAttributes) GetVolumeInfo(ref *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	s, err := a.backend.cachedCapacity(a.backend.root)
	if err != nil {
		return ftpWindowsError(err)
	}
	if err := a.ftpWindowsBehaviour.GetVolumeInfo(ref, info); err != nil {
		return err
	}
	info.TotalSize = s.Blocks * s.FragmentSize
	info.FreeSize = s.BlocksAvail * s.FragmentSize
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

func openRootCopySource(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
