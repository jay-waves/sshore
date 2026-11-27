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
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	sftp "github.com/pkg/sftp/v2"
)

type browseLocation struct{ host, dir string }

func parseBrowseLocation(a cliArguments) (browseLocation, error) {
	loc := browseLocation{host: "here"}
	arg := a.operand(0)
	if arg != "" {
		var err error
		if isBareSSHHost(arg) {
			err = validateHostOperand(a.command, arg)
			loc.host = arg
		} else {
			loc.host, loc.dir, err = splitEndpoint(arg)
		}
		if err != nil {
			return loc, err
		}
	}
	if loc.host == "origin" {
		return loc, usage("browse here: or an SSH host")
	}
	if a.command == "lp" && loc.dir != "" {
		return loc, usage("lp expects a host without a directory")
	}
	if loc.host == "here" && len(a.ssh) > 0 {
		return loc, usage("local browsing does not accept SSH options")
	}
	return loc, nil
}

func runFileBrowser(a cliArguments) (int, error) {
	loc, err := parseBrowseLocation(a)
	if err != nil {
		return 2, err
	}
	var action byte
	var directory, windowsHost bool
	var selected string
	ctx := context.Background()
	err = termui.RunLocalConsole(func(c *termui.LocalConsole) error {
		pick := func(opts termui.LFOptions) error {
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
			return pick(localFileSelection(dir, title, false))
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
	source := sshio.FormatEndpoint(loc.host, selected)
	attached := os.Getenv("SSHORE_SOCKET") != ""
	switch action {
	case 's':
		// Windows SFTP exposes drive paths with a leading slash.
		if windowsHost && len(selected) > 3 && selected[0] == '/' && selected[2] == ':' && selected[3] == '/' {
			source = sshio.FormatEndpoint(loc.host, selected[1:])
		}
		return browseCommand("shell", a.ssh, source)
	case 'f':
		return browseCommand("lp", a.ssh, sshio.FormatEndpoint(loc.host, ""))
	case 'm':
		if loc.host != "here" {
			return browseCommand("mount", a.ssh, source, ".:")
		}
		if attached {
			return browseCommand("mount", nil, source, "origin:")
		}
		remote, err := browsePrompt("Remote directory (HOST:PATH): ", "")
		if err != nil {
			return 1, err
		}
		return browseCommand("mount", a.ssh, remote, source)
	case 'c':
		target := "here:"
		if loc.host == "here" {
			if attached {
				target = "origin:"
			} else {
				target, err = browsePrompt("Copy to (HOST:[PATH]): ", "")
				if err != nil {
					return 1, err
				}
			}
		}
		args := []string{"--lf", source, target}
		if directory {
			args = append(args, "-r")
		}
		return browseCommand("copy", a.ssh, args...)
	}
	return 1, fmt.Errorf("unknown file action")
}

func browsePrompt(prompt, initial string) (string, error) {
	var value string
	err := termui.RunLocalConsole(func(*termui.LocalConsole) error {
		input, err := newLocalInput()
		if err != nil {
			return err
		}
		value, err = input.ReadPrefilled(prompt, false, initial)
		return errors.Join(err, input.Close())
	})
	return value, err
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
