package main

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
	xterm "golang.org/x/term"
)

func requireFileSelectionTerminal() error {
	if !xterm.IsTerminal(int(os.Stdin.Fd())) || !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return usage("file selection requires an interactive terminal; specify the paths")
	}
	return nil
}

func localFileSelection(dir, title string, directory bool) termui.LFOptions {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return termui.LFOptions{
		Dir: dir, Title: title, Directory: directory,
		Join: func(dir, name string) string { return filepath.Join(dir, name) }, Parent: filepath.Dir,
		ReadDir: func(_ context.Context, name string) ([]termui.LFEntry, error) {
			return fileSelectionEntries(os.ReadDir(name))
		},
	}
}

func remoteFileSelection(dir, title string, directory bool, readDir func(context.Context, string) ([]termui.LFEntry, error)) termui.LFOptions {
	return termui.LFOptions{
		Dir: dir, Title: title, Directory: directory,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir, ReadDir: readDir,
	}
}

func fileSelectionEntries(entries []fs.DirEntry, err error) ([]termui.LFEntry, error) {
	if err != nil {
		return nil, err
	}
	list := make([]termui.LFEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") {
			list = append(list, termui.LFEntry{Name: name, IsDir: entry.IsDir()})
		}
	}
	return list, nil
}
