package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
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
	mu      sync.Mutex
	source  *Source
	target  string
	size    int64
	direct  bool
	uploads map[string]*copyUpload
	ctx     context.Context
}

type copyUpload struct {
	mu    sync.RWMutex
	size  int64
	file  *os.File
	state uploadState
}

type uploadState uint8

const (
	uploadWriting uploadState = iota
	uploadSealed
	uploadVerified
	uploadDiscarding
)

func (f *FS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if f.direct {
		file, err := OpenSource(r.Filepath)
		if err != nil {
			return nil, err
		}
		return file.file, nil
	}
	if r.Filepath == "/source" && f.source != nil {
		info, err := f.source.file.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() != f.source.size {
			return nil, fmt.Errorf("source metadata changed")
		}
		// Individual SFTP handles may close before the operation ends.
		return struct{ io.ReaderAt }{f.source.file}, nil
	}
	return nil, os.ErrPermission
}

func (f *FS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flags := r.Pflags()
	if f.uploads == nil || !flags.Write || !flags.Creat || !flags.Excl || flags.Read || flags.Append {
		return nil, os.ErrPermission
	}
	target, size := f.target, f.size
	if f.direct {
		if len(f.uploads) >= 32 || f.uploads[r.Filepath] != nil || !strings.HasPrefix(filepath.Base(r.Filepath), ".sshore-") {
			return nil, os.ErrPermission
		}
		target, size = r.Filepath, -1
	} else if target == "" || size < 0 || len(f.uploads) != 0 || !strings.HasPrefix(r.Filepath, "/.sshore-") || strings.Contains(r.Filepath[1:], "/") {
		return nil, os.ErrPermission
	}
	file, err := stageCopyFile(target)
	if err != nil {
		return nil, err
	}
	u := &copyUpload{size: size, file: file}
	f.uploads[r.Filepath] = u
	return u, nil
}

func (f *FS) Filecmd(r *sftp.Request) error {
	f.mu.Lock()
	u := f.uploads[r.Filepath]
	f.mu.Unlock()
	if u == nil {
		return os.ErrPermission
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if r.Method == "Remove" {
		return f.discardUploadLocked(r.Filepath, u)
	}
	if u.state == uploadDiscarding {
		return os.ErrNotExist
	}
	ctx := f.ctx
	if ctx == nil {
		ctx = r.Context()
	}
	switch r.Method {
	case "Setstat":
		// The private temporary file is already 0600; no other mutation is allowed.
		attrs := r.Attributes()
		flags := r.AttrFlags()
		if flags.Size || flags.UidGid || flags.Acmodtime {
			return os.ErrPermission
		}
		if flags.Permissions {
			if len(attrs.Extended) == 0 && attrs.Mode == 0600 {
				return nil
			}
			return os.ErrPermission
		}
		if len(attrs.Extended) != 1 || attrs.Extended[0].ExtType != copyVerifyAttribute {
			return os.ErrPermission
		}
		return u.verify(ctx, attrs.Extended[0].ExtData)
	case "Link":
		if u.state != uploadVerified {
			return fmt.Errorf("upload is not verified")
		}
		target := f.target
		if f.direct {
			if filepath.Dir(r.Target) != filepath.Dir(u.file.Name()) {
				return os.ErrPermission
			}
			if err := ValidateName(filepath.Base(r.Target)); err != nil {
				return err
			}
			target = r.Target
		} else if r.Target != "/target" {
			return os.ErrPermission
		}
		defer f.discardUploadLocked(r.Filepath, u)
		return publishCopyFile(ctx, u.file, target, u.size)
	}
	return os.ErrPermission
}

// Keep failed removals registered for retry. Approved streams keep their one
// upload slot reserved even after its temporary file is gone.
func (f *FS) discardUploadLocked(path string, u *copyUpload) error {
	if err := u.discardLocked(); err != nil {
		return err
	}
	if f.direct {
		f.mu.Lock()
		if f.uploads[path] == u {
			delete(f.uploads, path)
		}
		f.mu.Unlock()
	}
	return nil
}

// The caller holds the upload lock. Keep the file path if removal needs a retry.
func (u *copyUpload) discardLocked() error {
	u.state = uploadDiscarding
	if u.file == nil {
		return nil
	}
	if err := discardCopyFile(u.file); err != nil {
		return err
	}
	u.file = nil
	return nil
}

func (f *FS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	info, err := f.stat(r)
	if err != nil {
		return nil, err
	}
	return transferInfo{info}, nil
}

func (f *FS) stat(r *sftp.Request) (os.FileInfo, error) {
	if r.Method != "Stat" && !(f.direct && r.Method == "Lstat") {
		return nil, os.ErrPermission
	}
	f.mu.Lock()
	upload := f.uploads[r.Filepath]
	f.mu.Unlock()
	if upload != nil {
		upload.mu.RLock()
		defer upload.mu.RUnlock()
		if upload.state == uploadDiscarding {
			return nil, os.ErrNotExist
		}
		return upload.file.Stat()
	}
	if f.direct {
		if r.Method == "Lstat" {
			return os.Lstat(r.Filepath)
		}
		return os.Stat(r.Filepath)
	}
	switch r.Filepath {
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

type transferInfo struct{ os.FileInfo }

func (infos transferInfo) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset != 0 || len(dst) == 0 {
		return 0, io.EOF
	}
	dst[0] = infos.FileInfo
	return 1, io.EOF
}

func (u *copyUpload) WriteAt(p []byte, off int64) (int, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.state != uploadWriting || off < 0 || u.size >= 0 && (off > u.size || int64(len(p)) > u.size-off) {
		return 0, os.ErrPermission
	}
	return u.file.WriteAt(p, off)
}
func (u *copyUpload) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == uploadWriting {
		u.state = uploadSealed
	}
	return nil
}
func (u *copyUpload) verify(ctx context.Context, sizeAndDigest string) error {
	if u.state == uploadWriting {
		return os.ErrPermission
	}
	sizeText, digestText, ok := strings.Cut(sizeAndDigest, ":")
	if !ok {
		return fmt.Errorf("invalid upload digest")
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 || u.size >= 0 && size != u.size {
		return fmt.Errorf("invalid upload size")
	}
	var expected [sha256.Size]byte
	if len(digestText) != hex.EncodedLen(len(expected)) {
		return fmt.Errorf("invalid upload digest")
	}
	if _, err := hex.Decode(expected[:], []byte(digestText)); err != nil {
		return fmt.Errorf("invalid upload digest")
	}
	info, err := u.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return fmt.Errorf("uploaded size mismatch")
	}
	hash := sha256.New()
	reader := io.NewSectionReader(u.file, 0, math.MaxInt64)
	if err := CopyExactSize(copyContextWriter{ctx, hash}, reader, size); err != nil {
		if err == errCopySizeChanged {
			return fmt.Errorf("uploaded size mismatch")
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), expected[:]) {
		return fmt.Errorf("uploaded content verification failed")
	}
	u.size, u.state = size, uploadVerified
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

func newCopyFS() *FS                                       { return &FS{direct: true, uploads: make(map[string]*copyUpload)} }
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
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend.ctx = ctx
	cwd := "/"
	if backend.direct {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	server := sftp.NewRequestServer(copyConn{conn, cancel}, sftp.Handlers{FileGet: backend, FilePut: backend, FileCmd: backend, FileList: backend}, sftp.WithStartDirectory(filepath.ToSlash(cwd)))
	defer server.Close()
	err := server.Serve()
	if err == io.EOF {
		return nil
	}
	return err
}

// NewSourceFS borrows the source until ServeTransfer returns.
func NewSourceFS(source *Source) *FS { return &FS{source: source} }
func NewTargetFS(path string, size int64) *FS {
	return &FS{target: path, size: size, uploads: make(map[string]*copyUpload)}
}
func (f *FS) Close() {
	f.mu.Lock()
	uploads := f.uploads
	f.uploads = nil
	f.mu.Unlock()
	for _, upload := range uploads {
		upload.mu.Lock()
		upload.discardLocked()
		upload.mu.Unlock()
	}
}
