package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/transport"
)

func runCopy(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, _, _, err := remoteCopyRequest(args); err != nil {
			return 2, err
		}
		return 0, serverCommand(append([]string{"cp"}, args...))
	}
	o, err := parseCopyCommand(args)
	if err != nil {
		return 2, err
	}
	if o.upload && o.sources[0].pick {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		termui.Log(os.Stdout, termui.Info, "To %s:%s. Submit source to copy.\n", cleanText(o.host), cleanText(o.target.path))
		source, err := readCopyPath(input, "Local file (Tab; Enter starts): ", "", func(p string) (string, error) {
			f, e := files.OpenSource(p)
			if e == nil {
				f.Close()
			}
			return p, e
		})
		closeErr := input.Close()
		if err != nil {
			return 1, err
		}
		if closeErr != nil {
			return 1, closeErr
		}
		o.sources = []copyEndpoint{{path: source}}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exe := sshExecutable()
	cmd := exec.CommandContext(ctx, exe, transport.SessionArgs(o.host, o.ssh, "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; "+remoteCommand(options{command: "cp-agent"}))...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	if err = cmd.Start(); err != nil {
		return 1, err
	}
	defer func() { cancel(); _ = stdin.Close(); _ = stdout.Close(); _ = cmd.Wait() }()
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close(); _ = stdout.Close() })
	defer stop()
	if err = negotiateSetupUsing(ctx, stdout, stdin, &options{command: "cp-agent", host: o.host}, setupClientUI{}); err != nil {
		return 1, err
	}
	client, err := files.NewSFTP(stdout, stdin)
	if err != nil {
		return 1, err
	}
	defer func() { _ = stdout.Close(); _ = stdin.Close(); _ = client.Close() }()
	if o.sources[0].stream {
		stopInput := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
		defer stopInput()
	}
	if o.sources[0].stream || o.target.stream {
		err = executeStreamCopy(client, o, os.Stdin, os.Stdout)
	} else {
		err = executeCopy(client, o)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Streams refer to this process, independently of origin endpoint selection.
func executeStreamCopy(client *sftp.Client, o copyOptions, input io.Reader, output io.Writer) error {
	if o.upload {
		if err := files.ValidateName(path.Base(o.target.path)); err != nil {
			return err
		}
		if _, err := client.Lstat(o.target.path); err == nil {
			return fmt.Errorf("stdin requires a new destination filename: %s", o.target.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return files.UploadReader(client, input, o.target.path, -1)
	}
	source := o.sources[0].path
	info, err := client.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("only ordinary files are supported")
	}
	return files.DownloadTo(client, source, output, info.Size(), nil)
}

func executeCopy(client *sftp.Client, o copyOptions) error {
	sourceStat, targetStat, targetLstat := client.Lstat, os.Stat, os.Lstat
	base, join := path.Base, filepath.Join
	transfer := files.Download
	if o.upload {
		sourceStat, targetStat, targetLstat = os.Lstat, client.Stat, client.Lstat
		base, join = filepath.Base, path.Join
		transfer = files.Upload
	}

	// Preflight the full batch and collect all destination choices before writing.
	type item struct {
		source, target string
		size           int64
	}
	var items []item
	var input *termui.Input
	if o.target.pick {
		var err error
		input, err = newLocalInput()
		if err != nil {
			return err
		}
		defer input.Close()
		termui.Log(os.Stdout, termui.Info, "From %s. Submit all destinations to copy; Ctrl-C cancels.\n", cleanText(o.host))
	}
	targetDir := false
	if !o.target.pick {
		info, err := targetStat(o.target.path)
		if err == nil {
			targetDir = info.IsDir()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(o.sources) > 1 && !targetDir {
			return fmt.Errorf("multiple sources require a destination directory")
		}
	}
	seen := map[string]bool{}
	for _, source := range o.sources {
		name := base(source.path)
		info, err := sourceStat(source.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("only ordinary files are supported")
		}
		if err = files.ValidateName(name); err != nil {
			return err
		}
		target := o.target.path
		if targetDir {
			target = join(target, name)
		}
		if input != nil {
			termui.Log(os.Stdout, termui.Info, "Copy %s:%s · %s\n", cleanText(o.host), cleanText(source.path), displayBytes(info.Size()))
			target, err = readCopyPath(input, "Save as (Tab; Enter accepts): ", "", func(p string) (string, error) { return files.DownloadDestination(p, name) })
			if err != nil {
				return err
			}
		}
		_, err = targetLstat(target)
		if err == nil {
			return fmt.Errorf("destination already exists: %s", target)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if seen[target] {
			return fmt.Errorf("duplicate destination: %s", target)
		}
		seen[target] = true
		items = append(items, item{source.path, target, info.Size()})
	}
	if input != nil {
		if err := input.Close(); err != nil {
			return err
		}
	}
	for i, item := range items {
		err := transfer(client, item.source, item.target, item.size, nil)
		if err != nil {
			return fmt.Errorf("%d completed, copy failed, %d not started: %w", i, len(items)-i-1, err)
		}
		termui.Log(os.Stderr, termui.Success, "Copied %s -> %s\n", cleanText(item.source), cleanText(item.target))
	}
	return nil
}
