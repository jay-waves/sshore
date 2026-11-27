package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/files/dirlist"
	"github.com/jay-waves/sshore/internal/termui"
	xterm "golang.org/x/term"
)

type directoryPairReader func(ctx context.Context, dir, parent string) ([]dirlist.Entry, error, []dirlist.Entry, error)

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
		Dir: dir, Title: title, Host: localFileHost(), Directory: directory, LocalFiles: true,
		Join: func(dir, name string) string { return filepath.Join(dir, name) }, Parent: filepath.Dir,
		ReadDir: func(_ context.Context, name string) ([]dirlist.Entry, error) {
			return dirlist.FromDirEntries(os.ReadDir(name))
		},
	}
}

func localFileHost() string {
	if host, _ := os.Hostname(); host != "" {
		return host + " (here)"
	}
	return "here"
}

func remoteFileSelection(dir, title, host string, directory bool, readDir func(context.Context, string) ([]dirlist.Entry, error)) termui.LFOptions {
	return termui.LFOptions{
		Dir: dir, Title: title, Host: host, Directory: directory,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir, ReadDir: readDir,
	}
}

func sftpFileSelection(dir, title, host string, directory bool, read files.DirectoryReader) termui.LFOptions {
	return remoteFileSelection(dir, title, host, directory, func(ctx context.Context, name string) ([]dirlist.Entry, error) {
		return dirlist.FromDirEntries(read(ctx, name))
	})
}

func pairedFileSelection(dir, title, host string, directory bool, read directoryPairReader) termui.LFOptions {
	picker := remoteFileSelection(dir, title, host, directory, func(ctx context.Context, dir string) ([]dirlist.Entry, error) {
		entries, err, _, _ := read(ctx, dir, "")
		return entries, err
	})
	picker.ReadPair = read
	return picker
}

func listingEntries(entries []fs.DirEntry, err error) ([]dirlist.Entry, string) {
	list, err := dirlist.FromDirEntries(entries, err)
	if err != nil {
		return nil, err.Error()
	}
	return list, ""
}

func listingError(message string) error {
	if message == "" {
		return nil
	}
	return errors.New(message)
}
