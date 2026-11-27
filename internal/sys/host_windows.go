package sys

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// CheckHost uses the real kernel version, independent of manifest shims.
func CheckHost() error {
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 || (version.MajorVersion == 10 && version.BuildNumber < 22000) {
		return fmt.Errorf("Windows 11 / Server 2025 or newer required")
	}
	return nil
}
