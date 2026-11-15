//go:build !windows

package termui

import "os"

func supportsANSI(f *os.File) bool { return true }
