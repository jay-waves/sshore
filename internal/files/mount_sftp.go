//go:build linux || windows || (darwin && arm64)

package files

import (
	"context"
	"io"

	sftp "github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"github.com/pkg/sftp/v2/localfs"
)

func serveMountData(data io.ReadWriteCloser) error {
	handler := &mountSFTPHandler{open: make(map[*mountSFTPFile]struct{})}
	defer func() {
		// The server only closes handles on explicit CLOSE requests.
		for file := range handler.open {
			_ = file.Close()
		}
	}()
	server := sftp.Server{Handler: handler, Debug: io.Discard, MaxDataLength: mountSFTPDataLength}
	return server.Serve(data)
}

// Serve calls the handler serially. Track both files and directories for disconnect cleanup.
type mountSFTPHandler struct {
	localfs.ServerHandler
	open map[*mountSFTPFile]struct{}
}

func (h *mountSFTPHandler) Open(ctx context.Context, req *sshfx.OpenPacket) (sftp.FileHandler, error) {
	file, err := h.ServerHandler.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	if opened, ok := file.(*localfs.File); ok {
		file = h.track(opened)
	}
	return file, nil
}

func (h *mountSFTPHandler) OpenDir(ctx context.Context, req *sshfx.OpenDirPacket) (sftp.DirHandler, error) {
	dir, err := h.ServerHandler.OpenDir(ctx, req)
	if err != nil {
		return nil, err
	}
	if opened, ok := dir.(*localfs.File); ok {
		dir = h.track(opened)
	}
	return dir, nil
}

func (h *mountSFTPHandler) track(file *localfs.File) *mountSFTPFile {
	tracked := &mountSFTPFile{File: file, owner: h}
	h.open[tracked] = struct{}{}
	return tracked
}

type mountSFTPFile struct {
	*localfs.File
	owner *mountSFTPHandler
}

func (f *mountSFTPFile) Close() error {
	delete(f.owner.open, f)
	return f.File.Close()
}
