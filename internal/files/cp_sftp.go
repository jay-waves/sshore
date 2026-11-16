package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/sftp"
)

const copyVerifyAttribute = "sha256@sshore"

// Each approved stream sees one virtual source or target, never a general
// filesystem. Its temporary upload is removed when the stream ends.
type FS struct {
	mu                      sync.RWMutex
	source, target, virtual string
	size                    int64
	upload                  *receivingFile
	direct                  bool
	transfers               map[string]*FS
	sealed, verified        bool
	ctx                     context.Context
}

func (f *FS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transfers != nil {
		file, err := OpenSource(r.Filepath)
		if err != nil {
			return nil, err
		}
		return file.file, nil
	}
	name := ""
	if f.upload != nil {
		name = f.upload.temp
	}
	if r.Filepath == "/source" && f.source != "" {
		name = f.source
	} else if r.Filepath != f.virtual || name == "" {
		return nil, os.ErrPermission
	}
	file, err := OpenSource(name)
	if err != nil {
		return nil, err
	}
	if file.size != f.size {
		file.Close()
		return nil, fmt.Errorf("source metadata changed")
	}
	return file.file, nil
}

func (f *FS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flags := r.Pflags()
	if f.transfers != nil {
		if len(f.transfers) >= 32 || f.transfers[r.Filepath] != nil || !flags.Write || !flags.Creat || !flags.Excl || flags.Read || flags.Append || !strings.HasPrefix(filepath.Base(r.Filepath), ".sshore-") {
			return nil, os.ErrPermission
		}
		file, err := newReceiving(r.Filepath, 0)
		if err != nil {
			return nil, err
		}
		child := &FS{direct: true, virtual: r.Filepath, size: -1, upload: file, ctx: f.ctx}
		f.transfers[r.Filepath] = child
		return transferWriter{child}, nil
	}
	if f.target == "" || f.upload != nil || !flags.Write || !flags.Creat || !flags.Excl || flags.Read || flags.Append || !strings.HasPrefix(r.Filepath, "/.sshore-") || strings.Contains(r.Filepath[1:], "/") {
		return nil, os.ErrPermission
	}
	file, err := newReceiving(f.target, f.size)
	if err != nil {
		return nil, err
	}
	f.upload, f.virtual = file, r.Filepath
	// Publication owns Sync/Close; SFTP CLOSE must not close this handle early.
	return transferWriter{f}, nil
}

func (f *FS) Filecmd(r *sftp.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transfers != nil {
		child := f.transfers[r.Filepath]
		if child == nil {
			return os.ErrPermission
		}
		err := child.Filecmd(r)
		if err == nil && r.Method == "Remove" {
			delete(f.transfers, r.Filepath)
		}
		return err
	}
	if f.upload == nil || r.Filepath != f.virtual {
		return os.ErrPermission
	}
	switch r.Method {
	case "Setstat":
		// The private temporary file is already 0600; no other mutation is allowed.
		attrs := r.Attributes()
		if len(attrs.Extended) != 0 {
			flags := r.AttrFlags()
			if flags.Size || flags.UidGid || flags.Permissions || flags.Acmodtime {
				return os.ErrPermission
			}
			ctx := f.ctx
			if ctx == nil {
				ctx = r.Context()
			}
			return f.verify(ctx, attrs.Extended)
		}
		flags := r.AttrFlags()
		if flags.Permissions && !flags.Size && !flags.UidGid && !flags.Acmodtime && r.Attributes().Mode == 0600 {
			return nil
		}
	case "Remove":
		f.upload.close()
		return nil
	case "Link":
		if !f.sealed || !f.verified {
			return fmt.Errorf("upload is not verified")
		}
		if f.direct {
			if filepath.Dir(r.Target) != filepath.Dir(f.virtual) {
				return os.ErrPermission
			}
			if err := ValidateName(filepath.Base(r.Target)); err != nil {
				return err
			}
			f.upload.target = r.Target
		} else if r.Target != "/target" {
			return os.ErrPermission
		}
		info, err := f.upload.file.Stat()
		if err != nil {
			return err
		}
		if info.Size() != f.size {
			return fmt.Errorf("incomplete file")
		}
		f.upload.done = info.Size()
		return f.upload.publish()
	}
	return os.ErrPermission
}

func (f *FS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transfers != nil {
		name := r.Filepath
		if child := f.transfers[name]; child != nil {
			name = child.upload.temp
		}
		var info os.FileInfo
		var err error
		if r.Method == "Lstat" {
			info, err = os.Lstat(name)
		} else if r.Method == "Stat" {
			info, err = os.Stat(name)
		} else {
			return nil, os.ErrPermission
		}
		return transferInfo{info}, err
	}
	name := ""
	switch r.Filepath {
	case "/source":
		name = f.source
	case "/target":
		name = f.target
	default:
		if r.Filepath == f.virtual && f.upload != nil {
			name = f.upload.temp
		}
	}
	if name == "" || r.Method != "Stat" {
		return nil, os.ErrPermission
	}
	info, err := os.Lstat(name)
	return transferInfo{info}, err
}

type transferInfo []os.FileInfo

func (infos transferInfo) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(infos)) {
		return 0, io.EOF
	}
	return copy(dst, infos[offset:]), io.EOF
}

type transferWriter struct{ fs *FS }

func (w transferWriter) WriteAt(p []byte, off int64) (int, error) {
	w.fs.mu.RLock()
	defer w.fs.mu.RUnlock()
	if w.fs.sealed || off < 0 || w.fs.size >= 0 && (off > w.fs.size || int64(len(p)) > w.fs.size-off) {
		return 0, os.ErrPermission
	}
	return w.fs.upload.file.WriteAt(p, off)
}
func (w transferWriter) Close() error {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	w.fs.sealed = true
	return nil
}
func (f *FS) verify(ctx context.Context, attrs []sftp.StatExtended) error {
	if !f.sealed || len(attrs) != 1 || attrs[0].ExtType != copyVerifyAttribute {
		return os.ErrPermission
	}
	parts := strings.Split(attrs[0].ExtData, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid upload digest")
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || size < 0 || f.size >= 0 && size != f.size {
		return fmt.Errorf("invalid upload size")
	}
	expected, err := hex.DecodeString(parts[1])
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("invalid upload digest")
	}
	info, err := f.upload.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return fmt.Errorf("uploaded size mismatch")
	}
	if err := f.upload.file.Sync(); err != nil {
		return err
	}
	hash := sha256.New()
	if err := CopyExactSize(copyContextWriter{ctx, hash}, io.NewSectionReader(f.upload.file, 0, size), size); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return fmt.Errorf("uploaded content verification failed")
	}
	f.size, f.upload.size = size, size
	f.verified = true
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

func newCopyFS() *FS                                       { return &FS{direct: true, transfers: make(map[string]*FS)} }
func (f *FS) Lstat(r *sftp.Request) (sftp.ListerAt, error) { return f.Filelist(r) }
func ServeCopy(conn io.ReadWriteCloser) error              { return ServeTransfer(conn, newCopyFS()) }

// Cancel hashing as soon as transport input ends, before SFTP waits for workers.
type copyConn struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (c copyConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if err != nil {
		c.cancel()
	}
	return n, err
}
func (c copyConn) Close() error { c.cancel(); return c.ReadWriteCloser.Close() }
func ServeTransfer(conn io.ReadWriteCloser, backend *FS) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend.ctx = ctx
	cwd := "/"
	if backend.transfers != nil {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	server := sftp.NewRequestServer(copyConn{conn, cancel}, sftp.Handlers{FileGet: backend, FilePut: backend, FileCmd: backend, FileList: backend}, sftp.WithStartDirectory(filepath.ToSlash(cwd)))
	defer backend.Close()
	defer server.Close()
	err := server.Serve()
	if err == io.EOF {
		return nil
	}
	return err
}

func NewSourceFS(path string, size int64) *FS { return &FS{source: path, size: size} }
func NewTargetFS(path string, size int64) *FS { return &FS{target: path, size: size} }
func (f *FS) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, child := range f.transfers {
		child.Close()
	}
	if f.upload != nil {
		f.upload.close()
	}
}
