//go:build linux || (darwin && arm64)

package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FUSE exposes remote modes/UIDs as the mounting user, while the SSH account
// remains the authority for remote access. Read caching lasts until close/reopen unless direct IO is selected;
// short attribute caches limit metadata round trips without a write-back queue.

func ValidateMountpoint(p string) error {
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

func (n *ftpNode) name() string { return n.Path(nil) }
func (n *ftpNode) childName(name string) string {
	// Preserve the components until remotePath validates them.
	return n.name() + "/" + name
}
func (n *ftpNode) remote() (string, error) { return n.backend.remotePath(n.name()) }
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
	p, e := n.backend.remotePath(n.childName(name))
	if e != nil {
		return nil, ftpErrno(e)
	}
	i, e := n.backend.lstat(p)
	if e != nil {
		return nil, ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return n.NewInode(ctx, &ftpNode{backend: n.backend}, fs.StableAttr{Mode: ftpMode(i)}), 0
}
func (n *ftpNode) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f, _ := h.(*ftpFuseFile)
	var p string
	if f == nil {
		var err error
		p, err = n.remote()
		if err != nil {
			return ftpErrno(err)
		}
	}
	return n.getattrAt(p, f, out)
}

func (n *ftpNode) getattrAt(p string, f *ftpFuseFile, out *fuse.AttrOut) syscall.Errno {
	var i os.FileInfo
	var e error
	if f != nil {
		i, e = f.file.Stat()
	} else {
		i, e = n.backend.lstat(p)
	}
	if e != nil {
		return ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return 0
}
func (n *ftpNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	p, e := n.remote()
	if e != nil {
		return nil, ftpErrno(e)
	}
	dir, e := n.backend.openDirectory(p)
	if e != nil {
		return nil, ftpErrno(e)
	}
	return &ftpFuseDirStream{ftpDirectory: dir}, 0
}

type ftpFuseDirStream struct{ ftpDirectory }

func (d *ftpFuseDirStream) HasNext() bool { return d.hasNext() }
func (d *ftpFuseDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	entry := d.take(1)[0]
	return fuse.DirEntry{Name: entry.Name(), Mode: ftpMode(entry), Off: uint64(d.offset)}, 0
}
func (d *ftpFuseDirStream) Close() { d.ftpDirectory = ftpDirectory{} }
func (d *ftpFuseDirStream) Seekdir(_ context.Context, off uint64) syscall.Errno {
	if off > uint64(len(d.entries)) {
		return syscall.EINVAL
	}
	d.offset = int(off)
	return 0
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
	return &ftpFuseFile{file: h}, n.openFlags(), 0
}
func (n *ftpNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	h, e := n.backend.open(n.childName(name), int(flags)|os.O_CREATE, os.FileMode(mode))
	if e != nil {
		return nil, nil, 0, ftpErrno(e)
	}
	i, e := h.Stat()
	if e != nil {
		h.Close()
		return nil, nil, 0, ftpErrno(e)
	}
	ftpAttr(i, &out.Attr)
	return n.NewInode(ctx, &ftpNode{backend: n.backend}, fs.StableAttr{Mode: ftpMode(i)}), &ftpFuseFile{file: h}, n.openFlags(), 0
}
func (n *ftpNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if e := n.backend.mkdir(n.childName(name), os.FileMode(mode)); e != nil {
		return nil, ftpErrno(e)
	}
	return n.Lookup(ctx, name, out)
}
func (n *ftpNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return ftpErrno(n.backend.remove(n.childName(name), false))
}
func (n *ftpNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return ftpErrno(n.backend.remove(n.childName(name), true))
}
func (n *ftpNode) Rename(ctx context.Context, name string, parent fs.InodeEmbedder, newname string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.ENOTSUP
	}
	p, ok := parent.(*ftpNode)
	if !ok {
		return syscall.EXDEV
	}
	return ftpErrno(n.backend.rename(n.childName(name), p.childName(newname)))
}

// A remote symlink must never resolve into the local machine outside the
// mount. Rewrite absolute targets within the selected remote tree to relative
// paths; reject escaping links. The SFTP account can still follow links remotely.
func (n *ftpNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	p, e := n.remote()
	if e != nil {
		return nil, ftpErrno(e)
	}
	target, e := n.backend.client.ReadLink(p)
	if e != nil {
		return nil, ftpErrno(e)
	}
	parent := path.Dir(p)
	resolved := target
	if !path.IsAbs(resolved) {
		resolved = path.Join(parent, resolved)
	}
	resolved = path.Clean(resolved)
	root := n.backend.root
	if resolved != root && !strings.HasPrefix(resolved, strings.TrimSuffix(root, "/")+"/") {
		return nil, syscall.EACCES
	}
	relative, e := filepath.Rel(parent, resolved)
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
	p, err := n.remote()
	if err != nil {
		return ftpErrno(err)
	}
	f, _ := h.(*ftpFuseFile)
	var opened *ftpFile
	if f != nil {
		opened = f.file
	}
	var update ftpAttributeUpdate
	if size, ok := in.GetSize(); ok {
		length := int64(size)
		update.size = &length
	}
	if mode, ok := in.GetMode(); ok {
		perm := os.FileMode(mode)
		update.mode = &perm
	}
	if at, ok := in.GetATime(); ok {
		update.atime = &at
	}
	if mt, ok := in.GetMTime(); ok {
		update.mtime = &mt
	}
	if err := n.backend.setAttributes(p, opened, update); err != nil {
		return ftpErrno(err)
	}
	return n.getattrAt(p, f, out)
}

type ftpFuseFile struct{ file *ftpFile }

func (f *ftpFuseFile) Read(ctx context.Context, b []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, e := f.file.ReadAt(b, off)
	if errors.Is(e, io.EOF) {
		e = nil
	}
	return fuse.ReadResultData(b[:n]), ftpErrno(e)
}
func (f *ftpFuseFile) Write(ctx context.Context, b []byte, off int64) (uint32, syscall.Errno) {
	n, e := f.file.WriteAt(b, off)
	return uint32(n), ftpErrno(e)
}
func (f *ftpFuseFile) Release(ctx context.Context) syscall.Errno { return ftpErrno(f.file.Close()) }
func (f *ftpFuseFile) Flush(ctx context.Context) syscall.Errno   { return 0 }
func (f *ftpFuseFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return ftpErrno(f.file.Sync())
}

var _ fs.NodeStatfser = (*ftpNode)(nil)

func (n *ftpNode) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	p, err := n.remote()
	if err != nil {
		return ftpErrno(err)
	}
	s, err := n.backend.cachedCapacity(p)
	if err != nil {
		return ftpErrno(err)
	}
	*out = fuse.StatfsOut{Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail,
		Files: s.Files, Ffree: s.Ffree, Bsize: uint32(s.Bsize), Frsize: uint32(s.Frsize), NameLen: uint32(s.Namemax)}
	return 0
}
