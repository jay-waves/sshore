//go:build linux && sshoretest

package main

import (
	"os"
	"path/filepath"
)

// Integration-test executables MUST NOT inspect or terminate the developer's
// sessions. This restriction is compiled in; a production environment variable
// cannot bypass the account-wide version policy.
func includeSetupProcess(p daemonProcess) bool {
	root := os.Getenv("SSHORE_RUNTIME_DIR")
	return filepath.IsAbs(root) && p.socket != "" && filepath.Dir(p.socket) == filepath.Clean(root)
}
