package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	sftp "github.com/pkg/sftp/v2"
)

type browseLocation struct{ host, dir string }

func (loc browseLocation) endpoint() string {
	return formatCLIEndpoint(loc.host, loc.dir)
}

func parseBrowseLocations(a cliArguments) (source, peer browseLocation, err error) {
	source.host = "here"
	switch len(a.operands) {
	case 1:
		source.host = a.operand(0)
		if err := validateHostOperand(a.command, source.host); err != nil {
			return source, peer, usage("use %s [HOST] or specify both endpoints", a.command)
		}
	case 2:
		source.host, source.dir, err = splitEndpoint(a.operand(0))
		if err != nil {
			return source, peer, err
		}
		peer.host, peer.dir, err = splitEndpoint(a.operand(1))
		if err != nil {
			return source, peer, err
		}
		if (source.host == "here") == (peer.host == "here") {
			return source, peer, usage("%s requires one local endpoint and one peer", a.command)
		}
		if a.command == "lp" && (source.dir != "" || peer.dir != "") {
			return source, peer, usage("lp endpoints take no value after the colon")
		}
		attached := os.Getenv("SSHORE_SOCKET") != ""
		if attached != (peer.host == "origin") {
			return source, peer, usage("%s pairs use .: and ..: inside an attached shell, or .: and an SSH host outside it", a.command)
		}
	}
	if source.host == "origin" {
		return source, peer, usage("browse here or an SSH host")
	}
	if source.host == "here" && (peer.host == "" || peer.host == "origin") && len(a.ssh) > 0 {
		return source, peer, usage("this browser has no SSH host for the SSH options")
	}
	return source, peer, nil
}

func runFileBrowser(a cliArguments) (int, error) {
	loc, peer, err := parseBrowseLocations(a)
	if err != nil {
		return 2, err
	}
	var action byte
	var directory, windowsHost bool
	var selected string
	ctx := context.Background()
	err = termui.RunLocalConsole(func(c *termui.LocalConsole) error {
		pick := func(opts termui.LFOptions) error {
			opts.BrowseTransfers = peer.host != ""
			opts.BrowseAction = func(key byte, isDir bool) { action, directory = key, isDir }
			var err error
			selected, err = c.PickLF(ctx, opts)
			return err
		}
		const title = "Files"
		if loc.host == "here" {
			dir, err := sys.Abs(cmp.Or(loc.dir, "."))
			if err != nil {
				return err
			}
			return pick(termui.LocalFileSelection(dir, title, false))
		}
		return withRemoteFiles(ctx, loc.host, a.ssh, func(client *sftp.Client, home string) error {
			windowsHost = len(home) > 3 && home[0] == '/' && home[2:4] == ":/"
			if windowsHost && len(loc.dir) > 2 && loc.dir[1] == ':' {
				loc.dir = "/" + strings.ReplaceAll(loc.dir, `\`, "/")
			}
			dir, err := files.ExpandRemoteHome(cmp.Or(loc.dir, "~"), home)
			if err != nil {
				return err
			}
			if !path.IsAbs(dir) {
				dir = path.Join(home, dir)
			}
			return pick(sftpFileSelection(dir, title, loc.host, false, files.NewSFTPDirectoryReader(client)))
		})
	})
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	source := formatCLIEndpoint(loc.host, selected)
	switch action {
	case 's':
		if loc.host == "here" {
			return browseCommand("shell", nil, source)
		}
		// Windows SFTP exposes drive paths with a leading slash.
		if windowsHost && len(selected) > 3 && selected[0] == '/' && selected[2] == ':' && selected[3] == '/' {
			source = formatCLIEndpoint(loc.host, selected[1:])
		}
		return browseCommand("shell", a.ssh, source)
	case 'm':
		if loc.host != "here" || peer.host == "origin" {
			return browseCommand("mount", a.ssh, source, peer.endpoint())
		}
		return browseCommand("mount", a.ssh, "--lf", peer.endpoint(), source)
	case 'c':
		args := []string{"--lf", source, peer.endpoint()}
		if directory {
			args = append(args, "-r")
		}
		return browseCommand("copy", a.ssh, args...)
	}
	return 1, fmt.Errorf("unknown file action")
}

func browseCommand(command string, ssh []string, args ...string) (int, error) {
	if len(ssh) > 0 {
		args = append(append(args, "--ssh"), ssh...)
	}
	parsed, err := parseCLI(args, command)
	if err != nil {
		return 2, err
	}
	return cliCommands[command].run(parsed)
}
