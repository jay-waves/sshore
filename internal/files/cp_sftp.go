package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"github.com/pkg/sftp/v2/encoding/ssh/filexfer/openssh"
)

// Each approved stream sees one virtual source or target, never a general
// filesystem. Its temporary upload is removed when the stream ends.
type FS struct {
	sftp.UnimplementedServerHandler
	nextHandle uint64
	cwd        string
	source     *Source
	target     string
	size       int64
	direct     bool
	uploads    map[string]*copyUpload
	ctx        context.Context
}

type copyUpload struct {
	size  int64
	limit int64
	file  *os.File
	state uploadState
}

type uploadState uint8

const (
	uploadWriting uploadState = iota
	uploadSealed
	uploadDiscarding
)

// Paths on approved streams are virtual. Direct copies use the remote working directory.
func (f *FS) resolve(name string) string {
	if f.direct {
		// Windows advertises its home as /C:/... so clients can use POSIX joins.
		if drivePath, ok := strings.CutPrefix(name, "/"); ok {
			native := filepath.FromSlash(drivePath)
			if len(filepath.VolumeName(native)) == 2 && filepath.IsAbs(native) {
				return filepath.Clean(native)
			}
		}
		if !path.IsAbs(name) && !filepath.IsAbs(filepath.FromSlash(name)) {
			name = path.Join(f.cwd, name)
		}
		return filepath.Clean(filepath.FromSlash(name))
	}
	return path.Join("/", name)
}

func (f *FS) Open(_ context.Context, req *sshfx.OpenPacket) (sftp.FileHandler, error) {
	name := f.resolve(req.Filename)
	flags := req.PFlags
	if flags == sshfx.FlagRead {
		if f.direct {
			source, err := OpenSource(name)
			if err != nil {
				return nil, err
			}
			return f.newHandle(source.file, nil, true), nil
		}
		if name == "/source" && f.source != nil {
			info, err := f.source.file.Stat()
			if err != nil {
				return nil, err
			}
			if info.Size() != f.source.size {
				return nil, fmt.Errorf("source metadata changed")
			}
			return f.newHandle(f.source.file, nil, false), nil
		}
	}
	const required = sshfx.FlagWrite | sshfx.FlagCreate | sshfx.FlagExclusive
	if f.uploads == nil || flags&required != required || flags&^uint32(required|sshfx.FlagTruncate) != 0 {
		return nil, os.ErrPermission
	}
	target, size := f.target, f.size
	if f.direct {
		if len(f.uploads) >= 32 || f.uploads[name] != nil || !strings.HasPrefix(filepath.Base(name), ".sshore-") {
			return nil, os.ErrPermission
		}
		target, size = name, -1
	} else if target == "" || size < 0 || len(f.uploads) != 0 || !strings.HasPrefix(name, "/.sshore-") || strings.Contains(name[1:], "/") {
		return nil, os.ErrPermission
	}
	limit := int64(math.MaxInt64)
	if size >= 0 {
		if size > math.MaxInt64-sha256.Size {
			return nil, fmt.Errorf("approved upload is too large")
		}
		limit = size + sha256.Size
	}
	file, err := stageCopyFile(target)
	if err != nil {
		return nil, err
	}
	upload := &copyUpload{size: size, limit: limit, file: file}
	f.uploads[name] = upload
	return f.newHandle(file, upload, false), nil
}

func (f *FS) Remove(_ context.Context, req *sshfx.RemovePacket) error {
	name := f.resolve(req.Path)
	upload := f.uploads[name]
	if upload == nil {
		return os.ErrPermission
	}
	return f.discardUpload(name, upload)
}

func (f *FS) Hardlink(_ context.Context, req *openssh.HardlinkExtendedPacket) error {
	name, destination := f.resolve(req.OldPath), f.resolve(req.NewPath)
	upload := f.uploads[name]
	if upload == nil || upload.state != uploadSealed {
		return os.ErrPermission
	}
	target := f.target
	if f.direct {
		if filepath.Dir(destination) != filepath.Dir(upload.file.Name()) {
			return os.ErrPermission
		}
		if err := ValidateName(filepath.Base(destination)); err != nil {
			return err
		}
		target = destination
	} else if destination != "/target" {
		return os.ErrPermission
	}
	if err := upload.verify(f.ctx); err != nil {
		return err
	}
	defer f.discardUpload(name, upload)
	return publishCopyFile(f.ctx, upload.file, target, upload.size)
}

func (f *FS) discardUpload(name string, upload *copyUpload) error {
	upload.state = uploadDiscarding
	if upload.file != nil {
		if err := discardCopyFile(upload.file); err != nil {
			return err
		}
		upload.file = nil
	}
	if f.direct {
		delete(f.uploads, name)
	}
	return nil
}

func (f *FS) Stat(_ context.Context, req *sshfx.StatPacket) (*sshfx.Attributes, error) {
	return copyAttributes(f.stat(f.resolve(req.Path), false))
}

func (f *FS) LStat(_ context.Context, req *sshfx.LStatPacket) (*sshfx.Attributes, error) {
	if !f.direct {
		return nil, os.ErrPermission
	}
	return copyAttributes(f.stat(f.resolve(req.Path), true))
}

func (f *FS) stat(name string, lstat bool) (os.FileInfo, error) {
	if upload := f.uploads[name]; upload != nil {
		if upload.state == uploadDiscarding {
			return nil, os.ErrNotExist
		}
		return upload.file.Stat()
	}
	if f.direct {
		if lstat {
			return os.Lstat(name)
		}
		return os.Stat(name)
	}
	switch name {
	case "/source":
		if f.source != nil {
			return f.source.file.Stat()
		}
	case "/target":
		if f.target != "" {
			return os.Lstat(f.target)
		}
	}
	return nil, os.ErrPermission
}

func copyAttributes(info os.FileInfo, err error) (*sshfx.Attributes, error) {
	if err != nil {
		return nil, err
	}
	var attrs sshfx.Attributes
	attrs.SetSize(uint64(info.Size()))
	attrs.SetPermissions(sshfx.FromGoFileMode(info.Mode()))
	return &attrs, nil
}

type copyFile struct {
	file   *os.File
	handle string
	upload *copyUpload
	owned  bool
}

func (f *FS) newHandle(file *os.File, upload *copyUpload, owned bool) *copyFile {
	f.nextHandle++
	return &copyFile{file: file, handle: strconv.FormatUint(f.nextHandle, 10), upload: upload, owned: owned}
}

func (f *copyFile) Name() string                     { return f.file.Name() }
func (f *copyFile) Handle() string                   { return f.handle }
func (f *copyFile) Stat() (*sshfx.Attributes, error) { return copyAttributes(f.file.Stat()) }
func (f *copyFile) ReadAt(p []byte, off int64) (int, error) {
	if f.upload != nil {
		return 0, os.ErrPermission
	}
	return f.file.ReadAt(p, off)
}
func (f *copyFile) WriteAt(p []byte, off int64) (int, error) {
	u := f.upload
	if u == nil || u.state != uploadWriting || off < 0 || off > u.limit || int64(len(p)) > u.limit-off {
		return 0, os.ErrPermission
	}
	return f.file.WriteAt(p, off)
}
func (f *copyFile) SetStat(*sshfx.Attributes) error { return os.ErrPermission }
func (f *copyFile) Sync() error                     { return os.ErrPermission }
func (f *copyFile) Close() error {
	if f.upload != nil && f.upload.state == uploadWriting {
		f.upload.state = uploadSealed
	}
	if f.owned {
		return f.file.Close()
	}
	return nil
}

// The final 32 bytes carry the sender's SHA-256. Verify the sealed payload
// on the receiving host, then strip the digest before publication.
func (u *copyUpload) verify(ctx context.Context) error {
	info, err := u.file.Stat()
	if err != nil {
		return err
	}
	size := info.Size() - sha256.Size
	if size < 0 || u.size >= 0 && size != u.size {
		return fmt.Errorf("uploaded size mismatch")
	}
	var expected [sha256.Size]byte
	if _, err := u.file.ReadAt(expected[:], size); err != nil {
		return err
	}
	digest := sha256.New()
	reader := io.NewSectionReader(u.file, 0, size)
	n, err := io.Copy(copyContextWriter{ctx, digest}, reader)
	if err != nil {
		return err
	}
	if n != size || !bytes.Equal(digest.Sum(nil), expected[:]) {
		return fmt.Errorf("uploaded content verification failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := u.file.Truncate(size); err != nil {
		return err
	}
	u.size = size
	return nil
}

type copyContextWriter struct {
	ctx context.Context
	io.Writer
}

func (w copyContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

func newCopyFS() *FS { return &FS{direct: true, uploads: make(map[string]*copyUpload)} }

func ServeCopy(conn io.ReadWriteCloser) error { return ServeTransfer(conn, newCopyFS()) }

func ServeTransfer(conn io.ReadWriteCloser, backend *FS) error {
	defer conn.Close()
	defer backend.Close()
	if backend.direct && backend.cwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		backend.cwd = filepath.ToSlash(cwd)
	}
	// Read transport input independently so a disconnect cancels verification
	// while the server is processing a request.
	input, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend.ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := io.Copy(writer, conn)
		cancel()
		_ = writer.CloseWithError(err)
	}()
	defer func() {
		_ = input.Close()
		_ = conn.Close()
		<-done
	}()
	server := sftp.Server{Handler: backend, Debug: io.Discard}
	return server.Serve(copyStream{ReadWriteCloser: conn, input: input})
}

type copyStream struct {
	io.ReadWriteCloser
	input io.Reader
}

func (c copyStream) Read(p []byte) (int, error) { return c.input.Read(p) }

// NewSourceFS borrows the source until ServeTransfer returns.
func NewSourceFS(source *Source) *FS { return &FS{source: source} }
func NewTargetFS(path string, size int64) *FS {
	return &FS{target: path, size: size, uploads: make(map[string]*copyUpload)}
}
func (f *FS) Close() {
	for name, upload := range f.uploads {
		_ = f.discardUpload(name, upload)
	}
}
