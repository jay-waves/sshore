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
}

type mountPathListing struct {
	Entries       []directoryEntry
	Error         string
	ParentEntries []directoryEntry
	ParentError   string
}

func readMountSelection(read func(any) error, selectPath func(mountPathRequest) error) error {
	for {
		var raw jsontext.Value
		if err := read(&raw); err != nil {
			return err
		}
		var request mountPathRequest
		if raw.Kind() != '{' || json.Unmarshal(raw, &request) != nil {
			return errors.New("invalid mount selection request")
		}
		if err := selectPath(request); err != nil {
			return err
		}
		if request.Directory == nil {
			return nil
		}
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
