package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/files"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func runCopy(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		kind, paths, hint, err := remoteCopyRequest(args)
		if err != nil {
			return 2, err
		}
		ops, err := prepareFileRequest(kind, paths, hint)
		if err != nil {
			return 1, err
		}
		return 0, requestOperations("cp", ops)
	}
	o, err := parseCopyCommand(args)
	if err != nil {
		return 2, err
	}
	if o.upload {
		for i := range o.sources {
			o.sources[i].path, err = termui.ExpandHomePath(o.sources[i].path)
			if err != nil {
				return 1, err
			}
		}
	} else if o.target.path, err = termui.ExpandHomePath(o.target.path); err != nil {
		return 1, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	var picked *files.Source
	defer closeCopySource(&picked)
	if o.upload && o.sources[0].pick {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		termui.Log(os.Stdout, termui.Info, "To %s:%s. Submit source to copy.\n", cleanText(o.host), cleanText(o.target.path))
		source, err := readCopyPath(input, "Local file (Tab; Enter starts): ", func(p string) (string, error) {
			var e error
			picked, e = files.OpenSource(p)
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
	connection := sshio.Options{Host: o.host, Args: o.ssh, Command: remoteCommand(options{command: "cp-agent", host: o.host}), Confirm: confirmSSHHost, Secret: promptSSHSecret}
	stream, err := sshio.Start(ctx, connection, os.Stderr)
	if err != nil {
		return 1, err
	}
	defer stream.CloseAndWait()
	remoteHome, err := negotiateSetup(stream, stream, o.host, false)
	if err != nil {
		return 1, err
	}
	if o.upload {
		o.target.path, err = files.ExpandRemoteHome(o.target.path, remoteHome)
		if err != nil {
			return 1, err
		}
	} else {
		for i := range o.sources {
			o.sources[i].path, err = files.ExpandRemoteHome(o.sources[i].path, remoteHome)
			if err != nil {
				return 1, err
			}
		}
	}
	if err := withCopySFTP(stream, func(client *sftp.Client) error {
		if o.sources[0].stream {
			stopInput := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
			defer stopInput()
		}
		if o.sources[0].stream || o.target.stream {
			return executeStreamCopy(client, o, os.Stdin, os.Stdout)
		}
		source := picked
		picked = nil // The batch now owns the selected source.
		return executeCopy(ctx, client, o, source)
	}); err != nil {
		return 1, err
	}
	return 0, nil
}

// Closing the full transport first releases SFTP reads and writes before
// client closure joins the protocol goroutines.
func withCopySFTP(stream io.ReadWriteCloser, run func(*sftp.Client) error) error {
	client, err := files.NewSFTP(stream, stream)
	if err != nil {
		_ = stream.Close()
		return err
	}
	defer func() { _ = stream.Close(); _ = client.Close() }()
	return run(client)
}

type fileCopy struct {
	source, target string
	downloadSize   int64
	opened         *files.Source
	approval       *operation // Original request for a scoped transfer.
}

func (f fileCopy) size() int64 {
	if f.opened != nil {
		return f.opened.Size()
	}
	if f.approval != nil {
		return f.approval.Size
	}
	return f.downloadSize
}

func closeCopySource(source **files.Source) {
	if *source != nil {
		(*source).Close()
		*source = nil
	}
}

func closeCopyBatch(batch []fileCopy) {
	for i := range batch {
		closeCopySource(&batch[i].opened)
	}
}

// A failed transfer leaves earlier files published. The caller owns cleanup
// for any sources still open when this returns.
func transferCopyBatch(batch []fileCopy, transfer func(int, fileCopy) error) (int, error) {
	for i, item := range batch {
		if err := transfer(i, item); err != nil {
			return i, err
		}
		closeCopySource(&batch[i].opened)
	}
	return len(batch), nil
}

func (f fileCopy) transfer(ctx context.Context, client *sftp.Client, progress *atomic.Int64) error {
	if f.opened != nil {
		return f.opened.Upload(client, f.target, progress)
	}
	return files.Download(ctx, client, f.source, f.target, f.size(), progress)
}

func prepareFileRequest(kind string, paths []string, hint string) ([]operation, error) {
	ops := make([]operation, 0, len(paths))
	for _, path := range paths {
		absolute, err := absoluteLocalPath(path)
		if err != nil {
			return nil, err
		}
		op := operation{Kind: kind, Path: absolute, LocalHint: hint}
		if kind == "download" {
			f, err := files.OpenSource(absolute)
			if err != nil {
				return nil, err
			}
			op.Name, op.Size = f.Name(), f.Size()
			f.Close()
		} else {
			info, err := os.Stat(absolute)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("upload destination must be a directory")
			}
		}
		ops = append(ops, op)
	}
	return ops, nil
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
	size, err := remoteCopySize(client, source)
	if err != nil {
		return err
	}
	return files.DownloadTo(client, source, output, size, nil)
}

func remoteCopySize(client *sftp.Client, source string) (int64, error) {
	info, err := client.Lstat(source)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("only ordinary files are supported")
	}
	return info.Size(), nil
}

func executeCopy(ctx context.Context, client *sftp.Client, o copyOptions, picked *files.Source) error {
	items := make([]fileCopy, len(o.sources))
	if picked != nil {
		items[0].opened = picked
	}
	defer closeCopyBatch(items)
	if err := prepareCopyBatch(client, o, items); err != nil {
		return err
	}
	completed, err := transferCopyBatch(items, func(_ int, item fileCopy) error {
		if err := item.transfer(ctx, client, nil); err != nil {
			return err
		}
		source, target := item.source, item.target
		if o.upload {
			target = o.host + ":" + target
		} else {
			source = o.host + ":" + source
		}
		termui.Log(os.Stderr, termui.Success, "Copied %s -> %s\n", cleanText(source), cleanText(target))
		return nil
	})
	if err != nil {
		return fmt.Errorf("%d completed, copy failed, %d not started: %w", completed, len(items)-completed-1, err)
	}
	return nil
}

// Resolve all paths and open local sources before handing the batch to execution.
// The caller owns cleanup, even if preparation fails partway through.
func prepareCopyBatch(client *sftp.Client, o copyOptions, items []fileCopy) (err error) {
	targetStat, targetLstat := os.Stat, os.Lstat
	join := filepath.Join
	if o.upload {
		targetStat, targetLstat = client.Stat, client.Lstat
		join = path.Join
	}

	var input *termui.Input
	var targetDir bool
	if o.target.pick {
		input, err = newLocalInput()
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := input.Close(); err == nil {
				err = closeErr
			}
		}()
		termui.Log(os.Stdout, termui.Info, "From %s. Submit all destinations to copy; Ctrl-C cancels.\n", cleanText(o.host))
	} else {
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
	for i, source := range o.sources {
		item := &items[i]
		item.source = source.path
		var name string
		if o.upload {
			if item.opened == nil {
				f, err := files.OpenSource(source.path)
				if err != nil {
					return err
				}
				item.opened = f
			}
			name = item.opened.Name()
		} else {
			size, err := remoteCopySize(client, source.path)
			if err != nil {
				return err
			}
			name, item.downloadSize = path.Base(source.path), size
			if err := files.ValidateName(name); err != nil {
				return err
			}
		}
		target := o.target.path
		if input != nil {
			termui.Log(os.Stdout, termui.Info, "Copy %s:%s · %s\n", cleanText(o.host), cleanText(source.path), displayBytes(item.size()))
			var err error
			target, err = readCopyPath(input, "Save as (Tab; Enter accepts): ", func(p string) (string, error) { return files.DownloadDestination(p, name) })
			if err != nil {
				return err
			}
		} else {
			if targetDir {
				target = join(target, name)
			}
			if o.upload {
				if err := files.ValidateName(path.Base(target)); err != nil {
					return err
				}
			}
			if err := files.CheckCopyTarget(target, targetLstat); err != nil {
				return err
			}
		}
		if seen[target] {
			return fmt.Errorf("duplicate destination: %s", target)
		}
		seen[target] = true
		item.target = target
	}
	return nil
}
