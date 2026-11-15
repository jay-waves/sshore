//go:build linux

package main

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var _ fs.NodeStatfser = (*ftpNode)(nil)

func (n *ftpNode) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	s, err := ftpRemoteCapacity(n.backend.client, n.remote())
	if err != nil {
		return ftpErrno(err)
	}
	*out = fuse.StatfsOut{Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail,
		Files: s.Files, Ffree: s.Ffree, Bsize: uint32(s.Bsize), Frsize: uint32(s.Frsize), NameLen: uint32(s.Namemax)}
	return 0
}
