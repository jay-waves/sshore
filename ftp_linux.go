//go:build linux

package main

// Linux exposes remote modes/UIDs as the mounting user, while the SSH account
// remains the authority for remote access. Read caching lasts until close/reopen unless direct IO is selected;
// short attribute caches limit metadata round trips without a write-back queue.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func validateFTPMountpoint(p string) error {
	info, e := os.Lstat(p)
	if e != nil {
		return fmt.Errorf("mountpoint must be an existing empty directory: %w", e)
	}
	if !info.IsDir() {
		return fmt.Errorf("mountpoint must be a directory, not a symlink or file")
	}
	entries, e := os.ReadDir(p)
	if e != nil {
		return e
	}
	if len(entries) != 0 {
		return fmt.Errorf("mountpoint must be empty")
	}
	return nil
}
func mountFTP(f *ftpFS, p string) (ftpMount, error) {
	timeout := time.Second
	opts := &fs.Options{AttrTimeout: &timeout, EntryTimeout: &timeout, MountOptions: fuse.MountOptions{FsName: "sshore", Name: "sshore", DisableXAttrs: true, MaxReadAhead: 128 * 1024}}
	if f.readOnly {
		opts.MountOptions.Options = []string{"ro"}
	}
	return fs.Mount(p, &ftpNode{backend: f}, opts)
}

type ftpNode struct {
	fs.Inode
	backend *ftpFS
}

func (n *ftpNode) name() string   { return n.Path(nil) }
func (n *ftpNode) remote() string { p, _ := n.backend.remotePath(n.name()); return p }
func ftpMode(i os.FileInfo) uint32 {
	m := uint32(i.Mode().Perm())
	switch {
	case i.IsDir():
		m |= syscall.S_IFDIR
	case i.Mode()&os.ModeSymlink != 0:
		m |= syscall.S_IFLNK
	case i.Mode().IsRegular():
		m |= syscall.S_IFREG
	default:
		m |= syscall.S_IFIFO
	}
	return m
}
func ftpAttr(i os.FileInfo, a *fuse.Attr) {
	a.Mode = ftpMode(i)
	a.Size = uint64(i.Size())
	a.Mtime = uint64(i.ModTime().Unix())
	a.Mtimensec = uint32(i.ModTime().Nanosecond())
	a.Atime = a.Mtime
	a.Ctime = a.Mtime
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getgid())
	a.Nlink = 1
	a.Blksize = 4096
	a.Blocks = (a.Size + 511) / 512
}
func (n *ftpNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	p := path.Join(n.remote(), name)
	i, e := n.backend.client.Lstat(p)
	if e != nil {
		return nil, ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return n.NewInode(ctx, &ftpNode{backend: n.backend}, fs.StableAttr{Mode: ftpMode(i)}), 0
}
func (n *ftpNode) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var i os.FileInfo
	var e error
	if f, ok := h.(*ftpLinuxFile); ok {
		i, e = f.file.Stat()
	} else {
		i, e = n.backend.client.Lstat(n.remote())
	}
	if e != nil {
		return ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return 0
}
func (n *ftpNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	h, e := n.backend.open(n.name(), os.O_RDONLY, 0)
	if e != nil {
		return nil, ftpErrno(e)
	}
	defer h.Close()
	list, e := h.Readdir(-1)
	if e != nil {
		return nil, ftpErrno(e)
	}
	entries := make([]fuse.DirEntry, 0, len(list))
	for _, i := range list {
		if i.Name() == "." || i.Name() == ".." {
			continue
		}
		entries = append(entries, fuse.DirEntry{Name: i.Name(), Mode: ftpMode(i)})
	}
	return fs.NewListDirStream(entries), 0
}

// Without KEEP_CACHE, reopening invalidates cached content. WritebackCache is
// not enabled: write callbacks still wait for the remote SFTP response.
func (n *ftpNode) openFlags() uint32 {
	if n.backend.directIO {
		return fuse.FOPEN_DIRECT_IO
	}
	return 0
}

func (n *ftpNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	h, e := n.backend.open(n.name(), int(flags)&^syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, 0, ftpErrno(e)
	}
	return &ftpLinuxFile{file: h}, n.openFlags(), 0
}
func (n *ftpNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	h, e := n.backend.open(path.Join(n.name(), name), int(flags)|os.O_CREATE, os.FileMode(mode))
	if e != nil {
		return nil, nil, 0, ftpErrno(e)
	}
	i, e := h.Stat()
	if e != nil {
		h.Close()
		return nil, nil, 0, ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return n.NewInode(ctx, &ftpNode{backend: n.backend}, fs.StableAttr{Mode: ftpMode(i)}), &ftpLinuxFile{file: h}, n.openFlags(), 0
}
func (n *ftpNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if e := n.backend.mkdir(path.Join(n.name(), name), os.FileMode(mode)); e != nil {
		return nil, ftpErrno(e)
	}
	return n.Lookup(ctx, name, out)
}
func (n *ftpNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return ftpErrno(n.backend.remove(path.Join(n.name(), name), false))
}
func (n *ftpNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return ftpErrno(n.backend.remove(path.Join(n.name(), name), true))
}
func (n *ftpNode) Rename(ctx context.Context, name string, parent fs.InodeEmbedder, newname string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.ENOTSUP
	}
	p, ok := parent.(*ftpNode)
	if !ok {
		return syscall.EXDEV
	}
	return ftpErrno(n.backend.rename(path.Join(n.name(), name), path.Join(p.name(), newname)))
}

// A remote symlink must never resolve into the local machine outside the
// mount. Rewrite absolute targets within the selected remote tree to relative
// paths; reject escaping links. The SFTP account can still follow links remotely.
func (n *ftpNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, e := n.backend.client.ReadLink(n.remote())
	if e != nil {
		return nil, ftpErrno(e)
	}
	resolved := target
	if !path.IsAbs(resolved) {
		resolved = path.Join(path.Dir(n.remote()), resolved)
	}
	resolved = path.Clean(resolved)
	root := n.backend.root
	if resolved != root && !strings.HasPrefix(resolved, strings.TrimSuffix(root, "/")+"/") {
		return nil, syscall.EACCES
	}
	relative, e := filepath.Rel(path.Dir(n.remote()), resolved)
	if e != nil {
		return nil, syscall.EIO
	}
	return []byte(relative), 0
}

func (n *ftpNode) Setattr(ctx context.Context, h fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if e := n.backend.writable(); e != nil {
		return ftpErrno(e)
	}
	if _, ok := in.GetUID(); ok {
		return syscall.ENOTSUP
	}
	if _, ok := in.GetGID(); ok {
		return syscall.ENOTSUP
	}
	p := n.remote()
	if size, ok := in.GetSize(); ok {
		var e error
		if f, ok := h.(*ftpLinuxFile); ok {
			e = f.file.Truncate(int64(size))
		} else {
			e = n.backend.client.Truncate(p, int64(size))
		}
		if e != nil {
			return ftpErrno(e)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if e := n.backend.client.Chmod(p, os.FileMode(mode)); e != nil {
			return ftpErrno(e)
		}
	}
	at, aok := in.GetATime()
	mt, mok := in.GetMTime()
	if aok || mok {
		info, e := n.backend.client.Stat(p)
		if e != nil {
			return ftpErrno(e)
		}
		if !aok {
			at = info.ModTime()
		}
		if !mok {
			mt = info.ModTime()
		}
		if e = n.backend.client.Chtimes(p, at, mt); e != nil {
			return ftpErrno(e)
		}
	}
	return n.Getattr(ctx, h, out)
}

type ftpLinuxFile struct{ file *ftpFile }

func (f *ftpLinuxFile) Read(ctx context.Context, b []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, e := f.file.ReadAt(b, off)
	if errors.Is(e, io.EOF) {
		e = nil
	}
	return fuse.ReadResultData(b[:n]), ftpErrno(e)
}
func (f *ftpLinuxFile) Write(ctx context.Context, b []byte, off int64) (uint32, syscall.Errno) {
	n, e := f.file.WriteAt(b, off)
	return uint32(n), ftpErrno(e)
}
func (f *ftpLinuxFile) Release(ctx context.Context) syscall.Errno { return ftpErrno(f.file.Close()) }
func (f *ftpLinuxFile) Flush(ctx context.Context) syscall.Errno   { return 0 }
func (f *ftpLinuxFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return ftpErrno(f.file.Sync())
}

// Keep /dev/tty accessible for password/MFA prompts. A background process
// group would stop those reads with SIGTTIN. OpenSSH honors inherited SIG_IGN
// for INT/QUIT after authentication. The fixed trampoline never interpolates
// user arguments; startup cancellation still kills the owned subprocess.
func isolateFTPCommand(cmd *exec.Cmd) {
	args := append([]string{cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", `trap '' INT QUIT; exec "$@"`, "sshore-ftp-ssh"}, args...)
}
