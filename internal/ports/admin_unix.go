//go:build !windows

package ports

import "os"

func isAdmin() bool { return os.Geteuid() == 0 }
