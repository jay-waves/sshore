package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	xterm "golang.org/x/term"
)

type directoryEntry struct {
	Name  string
	IsDir bool
}

type directoryPairReader func(ctx context.Context, dir, parent string) ([]directoryEntry, error, []directoryEntry, error)

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
		ReadDir: func(_ context.Context, name string) ([]termui.LFEntry, error) {
			return pickerEntries(directoryEntries(os.ReadDir(name)))
		},
	}
}

func localFileHost() string {
	if host, _ := os.Hostname(); host != "" {
		return host + " (here)"
	}
	return "here"
}

func remoteFileSelection(dir, title, host string, directory bool, readDir func(context.Context, string) ([]termui.LFEntry, error)) termui.LFOptions {
	return termui.LFOptions{
		Dir: dir, Title: title, Host: host, Directory: directory,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir, ReadDir: readDir,
	}
}

func pairedFileSelection(dir, title, host string, directory bool, read directoryPairReader) termui.LFOptions {
	picker := remoteFileSelection(dir, title, host, directory, func(ctx context.Context, dir string) ([]termui.LFEntry, error) {
		entries, err, _, _ := read(ctx, dir, "")
		return pickerEntries(entries, err)
	})
	picker.ReadPair = func(ctx context.Context, dir, parent string) ([]termui.LFEntry, error, []termui.LFEntry, error) {
		entries, err, parents, parentErr := read(ctx, dir, parent)
		current, err := pickerEntries(entries, err)
		siblings, parentErr := pickerEntries(parents, parentErr)
		return current, err, siblings, parentErr
	}
	return picker
}

func directoryEntries(entries []fs.DirEntry, err error) ([]directoryEntry, error) {
	if err != nil {
		return nil, err
	}
	list := make([]directoryEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") {
			list = append(list, directoryEntry{Name: name, IsDir: entry.IsDir()})
		}
	}
	return list, nil
}

func pickerEntries(entries []directoryEntry, err error) ([]termui.LFEntry, error) {
	if entries == nil {
		return nil, err
	}
	list := make([]termui.LFEntry, len(entries))
	for i, entry := range entries {
		list[i] = termui.LFEntry(entry)
	}
	return list, err
}

func listingEntries(entries []fs.DirEntry, err error) ([]directoryEntry, string) {
	list, err := directoryEntries(entries, err)
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

func confirmRemotePath(input prefilledPromptReader, prompt, initial, home string, check func(string) error) (string, error) {
	if cleanText(initial) != initial {
		return "", fmt.Errorf("invalid suggested remote path")
	}
	for {
		value, err := input.ReadPrefilled(prompt, false, initial)
		if err != nil {
			return "", err
		}
		name, err := files.ExpandRemoteHome(value, home)
		if err == nil && (name == "" || strings.ContainsRune(name, 0)) {
			err = fmt.Errorf("enter a remote path")
		}
		if err == nil && check != nil {
			err = check(name)
		}
		if err == nil {
			return name, nil
		}
		termui.Log(os.Stderr, "", termui.Warning, "%s\n", cleanText(err.Error()))
	}
}
