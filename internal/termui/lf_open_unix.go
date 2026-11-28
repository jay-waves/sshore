//go:build !windows

package termui

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
)

func openLFPath(ctx context.Context, name string) error {
	name, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.CommandContext(ctx, opener, name).Run()
}
