package files

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sftp "github.com/pkg/sftp/v2"
)

// NewDirectoryFS exposes one opened directory through a read or write grant.
// The caller releases the root after the transfer has cleaned up its uploads.
func NewDirectoryFS(name string, write bool) (*FS, func(), error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("approved path must be a directory")
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("approved directory changed; request again")
	}
	f := &FS{root: root, writeTree: write}
	if write {
		f.uploads = make(map[string]*copyUpload)
	}
	return f, func() { _ = root.Close() }, nil
}

func treePath(name string) string {
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." {
		return "."
	}
	for part := range strings.SplitSeq(name, "/") {
		if ValidateName(part) != nil {
			return ""
		}
	}
	return filepath.FromSlash(name)
}

func (f *FS) openTreeEntry(name string, directory bool) (*os.File, error) {
	info, err := f.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !(directory && info.IsDir() || !directory && info.Mode().IsRegular()) {
		return nil, os.ErrPermission
	}
	file, err := openRootCopySource(f.root, name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, os.ErrPermission
	}
	return file, nil
}

func (f *FS) stageTreeUpload(target string) (*os.File, string, error) {
	temp := filepath.Join(filepath.Dir(target), ".sshore-"+rand.Text()+".part")
	file, err := f.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	return file, temp, err
}

type copyTreeDir struct {
	sftp.DirHandler
	handle string
}

func (d copyTreeDir) Handle() string { return d.handle }
