//go:build !linux && !windows

package main

import "fmt"

func flushTerminalInput() error {
	return fmt.Errorf("local approval supports Linux and Windows terminals")
}
