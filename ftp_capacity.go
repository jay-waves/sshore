package main

import (
	"math"
	"syscall"

	"github.com/pkg/sftp"
)

type ftpCapacityClient interface {
	HasExtension(string) (string, bool)
	StatVFS(string) (*sftp.StatVFS, error)
}

func ftpRemoteCapacity(client ftpCapacityClient, root string) (*sftp.StatVFS, error) {
	version, ok := client.HasExtension("statvfs@openssh.com")
	if !ok || version != "2" {
		return nil, syscall.ENOTSUP
	}
	stat, err := client.StatVFS(root)
	if err != nil {
		return nil, err
	}
	if err := validateFTPCapacity(stat); err != nil {
		return nil, err
	}
	return stat, nil
}

func validateFTPCapacity(s *sftp.StatVFS) error {
	// Validate before narrowing FUSE fields or converting block counts to bytes.
	if s == nil || s.Frsize == 0 || s.Bsize == 0 ||
		s.Frsize > math.MaxUint32 || s.Bsize > math.MaxUint32 || s.Namemax > math.MaxUint32 ||
		s.Blocks > math.MaxUint64/s.Frsize || s.Bfree > s.Blocks || s.Bavail > s.Bfree {
		return syscall.EIO
	}
	return nil
}
