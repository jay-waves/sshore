package files

// SFTP transfers verify exact sizes; uploads also verify stored SHA-256.
// The receiver creates a temporary file in the destination directory, then uses
// a hard link for atomic publication without overwriting. There is deliberately
// no overwrite-prone fallback on filesystems without hard-link support.
// Normal cancellation removes temporary files; process death or a crash can leave
// .sshore-*.part files. Metadata is not copied: Linux files default to 0600 and
// Windows files inherit the destination directory ACL.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	sftp "github.com/pkg/sftp/v2"
)

func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, `/\:<>"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("unsafe filename %q", name)
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return fmt.Errorf("control character in filename")
		}
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(stem)
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return fmt.Errorf("reserved filename %q", name)
	}
	if (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && strings.Contains("123456789¹²³", stem[3:]) && utf8.RuneCountInString(stem[3:]) == 1 {
		return fmt.Errorf("reserved filename %q", name)
	}
	return nil
}

// ExpandRemoteHome interprets a home prefix using the Linux SSH peer's home.
func ExpandRemoteHome(name, home string) (string, error) {
	if name != "~" && !strings.HasPrefix(name, "~/") {
		return name, nil
	}
	if !path.IsAbs(home) || strings.ContainsRune(home, 0) {
		return "", fmt.Errorf("remote home directory is unavailable")
	}
	if name == "~" {
		return home, nil
	}
	return strings.TrimRight(home, "/") + name[1:], nil
}

type Source struct {
	file *os.File
	name string
	size int64
}

func OpenSource(path string) (*Source, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("only ordinary files are supported")
	}
	original := info
	f, err := openSourceFile(path)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(original, info) {
		f.Close()
		return nil, fmt.Errorf("not an ordinary file")
	}
	name := filepath.Base(path)
	if err := ValidateName(name); err != nil {
		f.Close()
		return nil, err
	}
	return &Source{file: f, name: name, size: info.Size()}, nil
}
func (f *Source) Close() { _ = f.file.Close() }

// CheckCopyTarget verifies that a local or SFTP destination has no entry,
// including a dangling symlink.
func CheckCopyTarget(target string, lstat func(string) (os.FileInfo, error)) error {
	_, err := lstat(target)
	if err == nil {
		return fmt.Errorf("destination already exists: %s", target)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func stageCopyFile(target string) (*os.File, error) {
	if err := CheckCopyTarget(target, os.Lstat); err != nil {
		return nil, err
	}
	return os.CreateTemp(filepath.Dir(target), ".sshore-*.part")
}

func discardCopyFile(file *os.File) error {
	_ = file.Close()
	err := os.Remove(file.Name())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func publishCopyFile(ctx context.Context, file *os.File, target string, size int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return fmt.Errorf("incomplete file")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The temporary file is on the same filesystem. Link fails if the
	// destination has appeared since preflight.
	if err := os.Link(file.Name(), target); err != nil {
		return fmt.Errorf("publish file without overwriting: %w", err)
	}
	return nil
}

var errCopySizeChanged = errors.New("source size changed")

// Read exactly the declared size and reject growth without size+1 overflow.
func CopyExactSize(dst io.Writer, src io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("invalid file size")
	}
	n, err := io.Copy(dst, io.LimitReader(src, size))
	if err != nil {
		return err
	}
	return checkCopySize(src, n, size)
}

func checkCopySize(src io.Reader, n, size int64) error {
	if n != size {
		return errCopySizeChanged
	}
	var extra [1]byte
	nread, err := io.ReadFull(src, extra[:])
	if nread != 0 {
		return errCopySizeChanged
	}
	if err != io.EOF {
		return err
	}
	return nil
}

// Upload transfers the file already opened by OpenSource.
func (f *Source) Upload(client *sftp.Client, target string, progress *atomic.Int64) error {
	return UploadReader(client, progressReader{f.file, progress}, target, f.size)
}

// A negative size denotes stdin: count bytes until EOF, then verify that count
// and digest before publishing. File sources still require their declared size.
func UploadReader(client *sftp.Client, source io.Reader, target string, size int64) (err error) {
	var nonce [16]byte
	rand.Read(nonce[:])
	temporary := path.Join(path.Dir(target), ".sshore-"+hex.EncodeToString(nonce[:])+".part")
	remote, err := client.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		// A successful Link transfers cleanup to the receiver. On failure it
		// may still own the temporary pathname, so ask it to discard again.
		if err != nil {
			_ = client.Remove(temporary)
		}
	}()
	digest := sha256.New()
	input := source
	if size >= 0 {
		input = io.LimitReader(source, size)
	}
	// Hash in source order; only the private temporary file receives concurrent writes.
	n, err := remote.ReadFrom(io.TeeReader(input, digest))
	if err == nil && size >= 0 {
		err = checkCopySize(source, n, size)
	}
	if err == nil {
		_, err = remote.WriteAt(digest.Sum(nil), n)
	}
	closeErr := remote.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = client.Link(temporary, target); err != nil {
		return fmt.Errorf("publish file without overwriting: %w", err)
	}
	return nil
}

func Download(ctx context.Context, client *sftp.Client, source, target string, size int64, progress *atomic.Int64) error {
	if size < 0 {
		return fmt.Errorf("invalid file size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receiver, err := stageCopyFile(target)
	if err != nil {
		return err
	}
	defer discardCopyFile(receiver)
	if err = DownloadTo(client, source, receiver, size, progress); err != nil {
		return err
	}
	return publishCopyFile(ctx, receiver, target, size)
}

func DownloadTo(client *sftp.Client, source string, target io.Writer, size int64, progress *atomic.Int64) error {
	sourceFile, err := client.Open(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return fmt.Errorf("source metadata changed")
	}
	return copySFTPExact(target, sourceFile, size, progress)
}

// Keep WriteTo visible so pkg/sftp pipelines reads, while bounding output even
// if the source grows after its metadata was checked. WriteTo drains its workers
// before returning; the destination and digest are written in file order.
func copySFTPExact(dst io.Writer, src *sftp.File, size int64, progress *atomic.Int64) error {
	if size < 0 {
		return fmt.Errorf("invalid file size")
	}
	writer := &exactCopyWriter{Writer: dst, remaining: size, progress: progress}
	if _, err := src.WriteTo(writer); err != nil {
		return err
	}
	if writer.remaining != 0 {
		return errCopySizeChanged
	}
	return nil
}

type exactCopyWriter struct {
	io.Writer
	remaining int64
	progress  *atomic.Int64
}

func (w *exactCopyWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errCopySizeChanged
	}
	n, err := w.Writer.Write(p)
	w.remaining -= int64(n)
	if w.progress != nil {
		w.progress.Add(int64(n))
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type progressReader struct {
	io.Reader
	count *atomic.Int64
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.count != nil {
		r.count.Add(int64(n))
	}
	return n, err
}

func (f *Source) Name() string { return f.name }
func (f *Source) Size() int64  { return f.size }

// DownloadDestination checks a local save path without creating it. Publication
// must independently enforce no-clobber semantics when writing.
func DownloadDestination(path, name string) (string, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, name)
	}
	if err := CheckCopyTarget(path, os.Lstat); err != nil {
		return "", err
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("destination parent is not a directory")
	}
	return path, nil
}
