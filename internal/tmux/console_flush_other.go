//go:build !linux && !windows

package tmux

import "fmt"

func FlushInput() error {
	return fmt.Errorf("local approval supports Linux and Windows terminals")
}
