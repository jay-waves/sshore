package main

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
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
	Entries       []directoryEntry
	Error         string
	ParentEntries []directoryEntry
	ParentError   string
}

// Both sides of the worker pipe accept listings until one final selection.
// Commit is a separate acknowledgement and cannot precede that selection.
func readMountStartup(read func(any) error, picking bool, selectPath func(mountPathRequest) error) error {
	for {
		var raw jsontext.Value
		if err := read(&raw); err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return errors.New("startup message too large")
		}
		if raw.Kind() == '"' {
			var answer string
			if picking || json.Unmarshal(raw, &answer) != nil || answer != "commit" {
				return errors.New("invalid startup acknowledgement")
			}
			return nil
		}
		var request mountPathRequest
		if !picking || raw.Kind() != '{' || json.Unmarshal(raw, &request) != nil {
			return errors.New("invalid mount selection request")
		}
		if err := selectPath(request); err != nil {
			return err
		}
		picking = request.Directory != nil
	}
}

func (o ftpOptions) pickMountPaths(ctx context.Context, home string, read directoryPairReader) (string, string, error) {
	remote, local := o.Remote, o.Mountpoint
	if o.PickRemote {
		var err error
		picker := pairedFileSelection(home, "Mount directory", o.Host, true, read)
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
	picker := localFileSelection(dir, "Mountpoint", true)
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
		return o.pickMountPaths(ctx, home, func(ctx context.Context, current, parent string) ([]directoryEntry, error, []directoryEntry, error) {
			entries, err := directoryEntries(readDir(ctx, current))
			if parent == "" {
				return entries, err, nil, nil
			}
			parents, parentErr := directoryEntries(readDir(ctx, parent))
			return entries, err, parents, parentErr
		})
	}
}
