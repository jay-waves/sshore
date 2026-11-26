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

	sftp "github.com/pkg/sftp/v2"

	"github.com/jay-waves/sshore/internal/files"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
)

func executeCopyCommand(o copyOptions) (int, error) {
	var err error
	interactive := o.target.pick
	for _, source := range o.sources {
		interactive = interactive || source.pick
	}
	if interactive {
		if err := requireFileSelectionTerminal(); err != nil {
			return 2, err
		}
	}
	if o.upload {
		for i := range o.sources {
			o.sources[i].path, err = sys.ExpandHome(o.sources[i].path)
			if err != nil {
				return 1, err
			}
		}
	} else if o.target.path, err = sys.ExpandHome(o.target.path); err != nil {
		return 1, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := remoteService(sshConnectionOptions(options{host: o.host, ssh: o.ssh}), "", remoteServiceRequest{Operation: "cp-agent"})
	connection = withSSHDiagnostics(connection, os.Stderr, nil)
	stream, err := sshio.Start(ctx, connection, os.Stderr)
	if err != nil {
		return 1, err
	}
	defer stream.CloseAndWait()
	remoteHome, err := negotiateFileSetup(stream)
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
	if err := withCopySFTP(ctx, stream, func(client *sftp.Client) error {
		if o.sources[0].stream {
			stopInput := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
			defer stopInput()
		}
		if o.sources[0].stream || o.target.stream {
			return executeStreamCopy(client, o, os.Stdin, os.Stdout)
		}
		return selectAndCopy(ctx, client, remoteHome, o)
	}); err != nil {
		return 1, err
	}
	return 0, nil
}

func selectAndCopy(ctx context.Context, client *sftp.Client, home string, o copyOptions) error {
	var readDir files.DirectoryReader
	if (o.upload && o.target.pick) || (!o.upload && o.sources[0].pick) {
		readDir = files.NewSFTPDirectoryReader(client)
		go readDir(ctx, home)
	}
	remote := func(title string, dir bool) termui.LFOptions {
		return remoteFileSelection(home, title, o.host, dir, func(ctx context.Context, name string) ([]termui.LFEntry, error) {
			return pickerEntries(directoryEntries(readDir(ctx, name)))
		})
	}
	var opened *files.Source
	defer closeCopySource(&opened)
	if o.sources[0].pick {
		picker := remote("Download file", false)
		if o.upload {
			picker = localFileSelection("", "Upload file", false)
			picker.Validate = func(name string) (err error) {
				opened, err = files.OpenSource(name)
				return err
			}
		}
		var err error
		o.sources[0].path, err = termui.PickLF(ctx, picker)
		if err != nil {
			return err
		}
	}
	if o.target.pick {
		picker := localFileSelection("", "Save in", true)
		if o.upload {
			picker = remote("Upload to", true)
		} else {
			picker.Manual = len(o.sources) == 1
		}
		var err error
		o.target.path, err = termui.PickLF(ctx, picker)
		if errors.Is(err, termui.ErrLFManual) {
			input, inputErr := newLocalInput()
			if inputErr != nil {
				return inputErr
			}
			o.target.path, err = readCopyPath(input, "Save as: ", func(p string) (string, error) {
				return files.DownloadDestination(p, path.Base(o.sources[0].path))
			})
			err = errors.Join(err, input.Close())
			o.target.pick = false
		}
		if err != nil {
			return err
		}
	}
	source := opened
	opened = nil // The batch now owns the selected source.
	return executeCopy(ctx, client, o, source)
}

// Closing the full transport first releases SFTP reads and writes before
// client closure joins the protocol goroutines.
func withCopySFTP(ctx context.Context, stream io.ReadWriteCloser, run func(*sftp.Client) error) error {
	client, err := files.NewCopyClient(ctx, stream)
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
		absolute, err := sys.Abs(path)
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
		if _, err := client.LStat(o.target.path); err == nil {
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
	info, err := client.LStat(source)
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
		termui.Log(os.Stderr, "", termui.Success, "Copied %s -> %s\n", cleanText(source), cleanText(target))
		return nil
	})
	if err != nil {
		return fmt.Errorf("%d completed, copy failed, %d not started: %w", completed, len(items)-completed-1, err)
	}
	return nil
}

// Resolve all paths and open local sources before handing the batch to execution.
// The caller owns cleanup, even if preparation fails partway through.
func prepareCopyBatch(client *sftp.Client, o copyOptions, items []fileCopy) error {
	targetStat, targetLstat := os.Stat, os.Lstat
	join := filepath.Join
	if o.upload {
		targetStat, targetLstat = client.Stat, client.LStat
		join = path.Join
	}

	var targetDir bool
	info, err := targetStat(o.target.path)
	if err == nil {
		targetDir = info.IsDir()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if (len(o.sources) > 1 || o.target.pick) && !targetDir {
		return fmt.Errorf("copy requires a destination directory")
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
		if seen[target] {
			return fmt.Errorf("duplicate destination: %s", target)
		}
		seen[target] = true
		item.target = target
	}
	return nil
}
