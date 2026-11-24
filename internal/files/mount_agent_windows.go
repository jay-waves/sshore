package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"github.com/pkg/sftp/v2/encoding/ssh/filexfer/openssh"
	"golang.org/x/sys/windows"
)

// Keep a leading slash in SFTP so clients can join paths independently of
// their operating system. The Windows service exposes drive-letter paths.
func mountWatchPath(name string) (string, error) {
	p, absolute := strings.CutPrefix(name, "/")
	p = filepath.FromSlash(p)
	if !absolute || len(p) < 2 || p[1] != ':' || len(filepath.VolumeName(p)) != 2 {
		return "", os.ErrInvalid
	}
	if len(p) == 2 {
		p += `\`
	}
	if !filepath.IsAbs(p) {
		return "", os.ErrInvalid
	}
	return filepath.Clean(p), nil
}

func mountWatchEvent(p string) string {
	return "/" + strings.TrimSuffix(filepath.ToSlash(p), "/")
}

func (h *mountSFTPHandler) RealPath(_ context.Context, req *sshfx.RealPathPacket) (string, error) {
	native := filepath.FromSlash(req.Path)
	if strings.HasPrefix(req.Path, "/") {
		var err error
		native, err = mountWatchPath(req.Path)
		if err != nil {
			return "", err
		}
	}
	if native == "" {
		native = "."
	}
	if !filepath.IsAbs(native) && (filepath.VolumeName(native) != "" || strings.HasPrefix(native, `\`)) {
		return "", os.ErrInvalid
	}
	native, err := filepath.Abs(native)
	if err != nil {
		return "", err
	}
	native, err = filepath.EvalSymlinks(native)
	if err != nil {
		return "", err
	}
	name, err := windowsMountUTF16(native)
	if err != nil {
		return "", err
	}
	volume := make([]uint16, 32768)
	if err := windows.GetVolumePathName(name, &volume[0], uint32(len(volume))); err != nil {
		return "", fmt.Errorf("identify mount volume: %w", err)
	}
	filesystem := make([]uint16, 256)
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return "", fmt.Errorf("identify mount filesystem: %w", err)
	}
	fs := windows.UTF16ToString(filesystem)
	if !strings.EqualFold(fs, "NTFS") && !strings.EqualFold(fs, "ReFS") {
		return "", fmt.Errorf("unsupported Windows filesystem %q; mounting requires NTFS or ReFS", fs)
	}
	resolved := mountWatchEvent(native)
	if _, err := mountWatchPath(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func (h *mountSFTPHandler) ReadLink(ctx context.Context, req *sshfx.ReadLinkPacket) (string, error) {
	target, err := h.ServerHandler.ReadLink(ctx, req)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) {
		target = mountWatchEvent(target)
		if _, err := mountWatchPath(target); err != nil {
			return "", err
		}
		return target, nil
	}
	if filepath.VolumeName(target) != "" || strings.HasPrefix(target, `\`) || strings.HasPrefix(target, "/") {
		return "", os.ErrInvalid
	}
	return filepath.ToSlash(target), nil
}

func (h *mountSFTPHandler) Remove(_ context.Context, req *sshfx.RemovePacket) error {
	name, err := mountWatchPath(req.Path)
	if err != nil {
		return err
	}
	native, err := windowsMountUTF16(name)
	if err != nil {
		return err
	}
	return windows.DeleteFile(native)
}

func (h *mountSFTPHandler) Rmdir(_ context.Context, req *sshfx.RmdirPacket) error {
	name, err := mountWatchPath(req.Path)
	if err != nil {
		return err
	}
	native, err := windowsMountUTF16(name)
	if err != nil {
		return err
	}
	return windows.RemoveDirectory(native)
}

func (h *mountSFTPHandler) Rename(_ context.Context, req *sshfx.RenamePacket) error {
	return renameWindowsMountPath(req.OldPath, req.NewPath, 0)
}

func (h *mountSFTPHandler) POSIXRename(_ context.Context, req *openssh.POSIXRenameExtendedPacket) error {
	return renameWindowsMountPath(req.OldPath, req.NewPath, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
}

func renameWindowsMountPath(oldPath, newPath string, flags uint32) error {
	oldName, err := mountWatchPath(oldPath)
	if err != nil {
		return err
	}
	newName, err := mountWatchPath(newPath)
	if err != nil {
		return err
	}
	source, err := windowsMountUTF16(oldName)
	if err != nil {
		return err
	}
	// Rename the entry itself, including when it is a symlink or junction.
	handle, err := windows.CreateFile(source, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	target, err := windows.UTF16FromString(`\\?\` + newName)
	if err != nil {
		return err
	}
	// FILE_RENAME_INFO carries the UTF-16 name in a variable-length tail.
	type fileRenameInfo struct {
		flags     uint32
		root      windows.Handle
		nameBytes uint32
		name      [1]uint16
	}
	var info fileRenameInfo
	size := int(unsafe.Offsetof(info.name)) + len(target)*2
	buffer := make([]byte, size)
	rename := (*fileRenameInfo)(unsafe.Pointer(&buffer[0]))
	rename.flags = flags
	rename.nameBytes = uint32((len(target) - 1) * 2)
	copy(unsafe.Slice(&rename.name[0], len(target)), target)
	return windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(size))
}

func windowsMountUTF16(name string) (*uint16, error) {
	return windows.UTF16PtrFromString(`\\?\` + name)
}

func (h *mountSFTPHandler) StatVFS(_ context.Context, req *openssh.StatVFSExtendedPacket) (*openssh.StatVFSExtendedReplyPacket, error) {
	native, err := mountWatchPath(req.Path)
	if err != nil {
		return nil, err
	}
	// The extended-length path preserves support for nested drive directories.
	name, err := windowsMountUTF16(native)
	if err != nil {
		return nil, err
	}
	stat := &openssh.StatVFSExtendedReplyPacket{BlockSize: 1, FragmentSize: 1, MaxNameLength: 255}
	if err := windows.GetDiskFreeSpaceEx(name, &stat.BlocksAvail, &stat.Blocks, &stat.BlocksFree); err != nil {
		return nil, err
	}
	// Windows reports volume-wide free bytes even when a caller quota limits total bytes.
	stat.BlocksFree = min(stat.BlocksFree, stat.Blocks)
	return stat, nil
}
