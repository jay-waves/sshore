//go:build linux

package files

import (
	"context"
	"path/filepath"

	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"golang.org/x/sys/unix"
)

func (h *mountSFTPHandler) RealPath(_ context.Context, req *sshfx.RealPathPacket) (string, error) {
	name := req.Path
	if name == "" {
		name = "."
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func (h *mountSFTPHandler) Remove(_ context.Context, req *sshfx.RemovePacket) error {
	return unix.Unlink(req.Path)
}

func (h *mountSFTPHandler) Rmdir(_ context.Context, req *sshfx.RmdirPacket) error {
	return unix.Rmdir(req.Path)
}
