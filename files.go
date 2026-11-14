package main

// SFTP transfers verify exact sizes; uploads also verify stored SHA-256.
// The receiver creates a temporary file in the destination directory, then uses
// a hard link for atomic publication without overwriting. There is deliberately
// no overwrite-prone fallback on filesystems without hard-link support.
// Normal cancellation removes temporary files; process death or a crash can leave
// .sshore-*.part files. Metadata is not copied: Linux files default to 0600 and
// Windows files inherit the destination directory ACL.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/pkg/sftp"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

func safeName(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, `/\:<>"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("unsafe filename %q", name)
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return fmt.Errorf("control character in filename")
		}
	}
	stem := strings.ToUpper(strings.Split(name, ".")[0])
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return fmt.Errorf("reserved filename %q", name)
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return fmt.Errorf("reserved filename %q", name)
	}
	return nil
}

type sendingFile struct {
	file *os.File
	name string
	size int64
}

func openSending(path string) (*sendingFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("only ordinary files are supported")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not an ordinary file")
	}
	name := filepath.Base(path)
	if err := safeName(name); err != nil {
		f.Close()
		return nil, err
	}
	return &sendingFile{file: f, name: name, size: info.Size()}, nil
}
func (f *sendingFile) close() { _ = f.file.Close() }

type receivingFile struct {
	file         *os.File
	temp, target string
	size, done   int64
}

func newReceiving(target string, size int64) (*receivingFile, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid file size")
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("destination already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".sshore-*.part")
	if err != nil {
		return nil, err
	}
	return &receivingFile{file: f, temp: f.Name(), target: target, size: size}, nil
}
func (f *receivingFile) close() { _ = f.file.Close(); _ = os.Remove(f.temp) }
func (f *receivingFile) put(b []byte) error {
	if int64(len(b)) > f.size-f.done {
		return fmt.Errorf("received more data than announced")
	}
	if err := writeFull(f.file, b); err != nil {
		return err
	}
	f.done += int64(len(b))
	return nil
}
func (f *receivingFile) Write(b []byte) (int, error) {
	if err := f.put(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (f *receivingFile) publish() error {
	defer f.close()
	if f.done != f.size {
		return fmt.Errorf("incomplete file")
	}
	if err := f.file.Sync(); err != nil {
		return err
	}
	if err := f.file.Close(); err != nil {
		return err
	}
	// Atomic no-clobber publication. Both paths are on the same filesystem.
	// Unlike Rename on Unix, Link cannot replace an existing destination.
	if err := os.Link(f.temp, f.target); err != nil {
		return fmt.Errorf("publish file without overwriting: %w", err)
	}
	return nil
}

// Read exactly the declared size and reject growth without size+1 overflow.
func copyExactSize(dst io.Writer, src io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("invalid file size")
	}
	n, err := io.Copy(dst, io.LimitReader(src, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("source size changed")
	}
	var extra [1]byte
	nread, err := io.ReadFull(src, extra[:])
	if nread != 0 {
		return fmt.Errorf("source size changed")
	}
	if err != io.EOF {
		return err
	}
	return nil
}

func copyUpload(client *sftp.Client, source, target string, size int64, progress *atomic.Int64) error {
	f, err := openSending(source)
	if err != nil {
		return err
	}
	defer f.close()
	if f.size != size {
		return fmt.Errorf("source size changed")
	}
	return copyUploadReader(client, progressReader{f.file, progress}, target, size)
}

// A negative size denotes stdin: count bytes until EOF, then verify that count
// and digest before publishing. File sources still require their declared size.
func copyUploadReader(client *sftp.Client, source io.Reader, target string, size int64) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(target), ".sshore-"+hex.EncodeToString(nonce[:])+".part")
	remote, err := client.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer client.Remove(temporary)
	defer remote.Close()
	if err = remote.Chmod(0600); err != nil {
		return err
	}
	digest := sha256.New()
	writer := io.MultiWriter(remote, digest)
	if size < 0 {
		size, err = io.Copy(writer, source)
	} else {
		err = copyExactSize(writer, source, size)
	}
	if err != nil {
		return err
	}
	if err = remote.Close(); err != nil {
		return err
	}
	// Verify stored content before atomic, no-overwrite publication.
	stored, err := client.Open(temporary)
	if err != nil {
		return err
	}
	verification := sha256.New()
	if err = errors.Join(copyExactSize(verification, stored, size), stored.Close()); err != nil {
		return err
	}
	if !bytes.Equal(digest.Sum(nil), verification.Sum(nil)) {
		return fmt.Errorf("uploaded content verification failed")
	}
	if err = client.Link(temporary, target); err != nil {
		return fmt.Errorf("publish file without overwriting: %w", err)
	}
	return nil
}

func copyDownload(client *sftp.Client, source, target string, size int64, progress *atomic.Int64) error {
	receiver, err := newReceiving(target, size)
	if err != nil {
		return err
	}
	defer receiver.close()
	if err = copyDownloadTo(client, source, receiver, size, progress); err != nil {
		return err
	}
	return receiver.publish()
}

func copyDownloadTo(client *sftp.Client, source string, target io.Writer, size int64, progress *atomic.Int64) error {
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
	return copyExactSize(target, progressReader{sourceFile, progress}, size)
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
