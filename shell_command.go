package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func runShellCommand(args []string) (int, error) {
	if code, handled, err := runTaskID(args); handled {
		return code, err
	}
	o, err := parseOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp(args[:1], os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	if o.command == "spawn" {
		o.temporary = true
		o.owner, err = currentSpawnOwner()
		if err != nil {
			return 1, err
		}
	}
	if o.local {
		return runLocalCommand(o)
	}
	if err = resolveRecentAttach(&o); err != nil {
		return 1, err
	}
	o.downloadDir, err = prepareDownloadDir(o.downloadDir)
	if err != nil {
		return 1, err
	}
	if o.list {
		return runInventory(o)
	}
	return runClient(o)
}

func prepareDownloadDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("download destination is not a directory: %s", path)
	}
	return path, nil
}
