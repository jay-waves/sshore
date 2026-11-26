package main

import (
	"context"
	"errors"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
)

type mountPathRequest struct {
	Directory *string
	Parent    string
	Remote    string
	Local     string
	Error     string
}

type mountPathListing struct {
	Entries       []termui.LFEntry
	Error         string
	ParentEntries []termui.LFEntry
	ParentError   string
}

func (o ftpOptions) pickMountPaths(ctx context.Context, home string, list func(context.Context, string) ([]termui.LFEntry, error), pair func(context.Context, string, string) ([]termui.LFEntry, error, []termui.LFEntry, error)) (string, string, error) {
	remote, local := o.Remote, o.Mountpoint
	if o.PickRemote {
		var err error
		picker := remoteFileSelection(home, "Mount directory · "+o.Host, true, list)
		picker.ReadPair = pair
		remote, err = termui.PickLF(ctx, picker)
		if err != nil {
			return "", "", err
		}
	}
	if o.PickMountpoint {
		var err error
		local, err = pickLocalMountpoint(ctx, "", false)
		if err != nil {
			return "", "", err
		}
	}
	return remote, local, nil
}

func pickLocalMountpoint(ctx context.Context, dir string, inConsole bool) (string, error) {
	picker := localFileSelection(dir, "Local mountpoint", true)
	picker.Automatic, picker.Manual, picker.InConsole = true, true, inConsole
	picker.Validate = files.ValidateMountpoint
	point, err := termui.PickLF(ctx, picker)
	if !errors.Is(err, termui.ErrLFManual) {
		return point, err
	}
	input, err := newLocalInput()
	if err != nil {
		return "", err
	}
	point, err = readMountApprovalPath(input, "")
	return point, errors.Join(err, input.Close())
}

func mountPathSelector(o ftpOptions) func(context.Context, files.DirectoryReader, string) (string, string, error) {
	return func(ctx context.Context, readDir files.DirectoryReader, home string) (string, string, error) {
		if o.PickRemote {
			go readDir(ctx, home)
		}
		return o.pickMountPaths(ctx, home, func(ctx context.Context, name string) ([]termui.LFEntry, error) {
			return fileSelectionEntries(readDir(ctx, name))
		}, func(ctx context.Context, current, parent string) ([]termui.LFEntry, error, []termui.LFEntry, error) {
			entries, err := fileSelectionEntries(readDir(ctx, current))
			parents, parentErr := fileSelectionEntries(readDir(ctx, parent))
			return entries, err, parents, parentErr
		})
	}
}
