package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/pkg/sftp"
)

// Each approved stream sees one virtual source or target, never a general
// filesystem. Its temporary upload is removed when the stream ends.
type transferFS struct {
	mu                      sync.Mutex
	source, target, virtual string
	size                    int64
	upload                  *receivingFile
}

func (f *transferFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := ""
	if f.upload != nil {
		name = f.upload.temp
	}
	if r.Filepath == "/source" && f.source != "" {
		name = f.source
	} else if r.Filepath != f.virtual || name == "" {
		return nil, os.ErrPermission
	}
	file, err := openSending(name)
	if err != nil {
		return nil, err
	}
	if file.size != f.size {
		file.close()
		return nil, fmt.Errorf("source metadata changed")
	}
	return file.file, nil
}

func (f *transferFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flags := r.Pflags()
	if f.target == "" || f.upload != nil || !flags.Write || !flags.Creat || !flags.Excl || flags.Read || flags.Append || !strings.HasPrefix(r.Filepath, "/.sshore-") || strings.Contains(r.Filepath[1:], "/") {
		return nil, os.ErrPermission
	}
	file, err := newReceiving(f.target, f.size)
	if err != nil {
		return nil, err
	}
	f.upload, f.virtual = file, r.Filepath
	// Publication owns Sync/Close; SFTP CLOSE must not close this handle early.
	return transferWriter{file.file}, nil
}

func (f *transferFS) Filecmd(r *sftp.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upload == nil || r.Filepath != f.virtual {
		return os.ErrPermission
	}
	switch r.Method {
	case "Setstat":
		// The private temporary file is already 0600; no other mutation is allowed.
		flags := r.AttrFlags()
		if flags.Permissions && !flags.Size && !flags.UidGid && !flags.Acmodtime && r.Attributes().Mode == 0600 {
			return nil
		}
	case "Remove":
		f.upload.close()
		return nil
	case "Link":
		if r.Target != "/target" {
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

func (f *transferFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

type transferWriter struct{ io.WriterAt }
