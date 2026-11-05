package main

// File transfer verifies size and SHA-256 before publishing a received file.
// The receiver creates a temporary file in the destination directory, then uses
// a hard link for atomic publication without overwriting. There is deliberately
// no overwrite-prone fallback on filesystems without hard-link support.
// Normal cancellation removes temporary files; process death or a crash can leave
// .sshore-*.part files. Metadata is not copied: Linux files default to 0600 and
// Windows files inherit the destination directory ACL.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	file       *os.File
	name       string
	size, done int64
	hash       hash.Hash
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
	return &sendingFile{file: f, name: name, size: info.Size(), hash: sha256.New()}, nil
}
func (f *sendingFile) close() { _ = f.file.Close() }
func (f *sendingFile) offer(id uint64) message {
	return message{Type: "offer", ID: id, Name: f.name, Size: f.size}
}
func (f *sendingFile) next(id uint64) (message, error) {
	b := make([]byte, chunkSize)
	n, err := f.file.Read(b)
	if n > 0 {
		f.done += int64(n)
		if f.done > f.size {
			return message{}, fmt.Errorf("file grew during transfer")
		}
		f.hash.Write(b[:n])
		return message{Type: "chunk", ID: id, Data: b[:n]}, nil
	}
	if err != io.EOF {
		if err == nil {
			err = io.ErrNoProgress
		}
		return message{}, err
	}
	if f.done != f.size {
		return message{}, fmt.Errorf("file size changed during transfer")
	}
	return message{Type: "end", ID: id, Hash: hex.EncodeToString(f.hash.Sum(nil))}, nil
}

type receivingFile struct {
	file         *os.File
	temp, target string
	size, done   int64
	hash         hash.Hash
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
	return &receivingFile{file: f, temp: f.Name(), target: target, size: size, hash: sha256.New()}, nil
}
func (f *receivingFile) close() { _ = f.file.Close(); _ = os.Remove(f.temp) }
func (f *receivingFile) put(b []byte) error {
	if int64(len(b)) > f.size-f.done {
		return fmt.Errorf("received more data than announced")
	}
	if err := writeFull(f.file, b); err != nil {
		return err
	}
	f.hash.Write(b)
	f.done += int64(len(b))
	return nil
}
func (f *receivingFile) finish(sum string) error {
	defer f.close()
	if f.done != f.size {
		return fmt.Errorf("incomplete file")
	}
	if hex.EncodeToString(f.hash.Sum(nil)) != sum {
		return fmt.Errorf("SHA-256 mismatch")
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
