package mount

import (
	"io"

	"github.com/pkg/sftp"
)

func CheckPlatform() error                                        { return checkFTPPlatform() }
func ValidateMountpoint(p string) error                           { return validateFTPMountpoint(p) }
func NewSFTP(r io.Reader, w io.WriteCloser) (*sftp.Client, error) { return newFTPSFTP(r, w) }
