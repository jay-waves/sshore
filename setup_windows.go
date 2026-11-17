package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func prepareReplacement(target string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Dir(target), target} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && path == target {
			continue
		}
		if err != nil {
			return "", err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", err
		}
		attributes, err := windows.GetFileAttributes(name)
		if err != nil {
			return "", err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return "", fmt.Errorf("installation path must not be a reparse point")
		}
		if info.Mode()&os.ModeSymlink != 0 || path == target && !info.Mode().IsRegular() || path != target && !info.IsDir() {
			return "", fmt.Errorf("installation target must be a regular executable in a real directory")
		}
	}
	return stageReplacement(target)
}
