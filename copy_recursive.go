package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	sftp "github.com/pkg/sftp/v2"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/termui"
)

type copyFilesystem struct {
	clean, base func(string) string
	join        func(...string) string
	lstat       func(string) (fs.FileInfo, error)
	dirFS       func(string) fs.FS
	mkdir       func(string, fs.FileMode) error
}

type recursiveCopyEntry struct {
	fileCopy
	directory bool
}

func executeRecursiveCopy(ctx context.Context, client *sftp.Client, o copyOptions, exactTarget bool) error {
	local := copyFilesystem{
		clean: filepath.Clean, base: filepath.Base, join: filepath.Join,
		lstat: os.Lstat, dirFS: os.DirFS, mkdir: os.Mkdir,
	}
	remote := copyFilesystem{
		clean: path.Clean, base: path.Base, join: path.Join,
		lstat: client.LStat, mkdir: client.Mkdir,
		dirFS: client.DirFS,
	}
	sourceFS, targetFS := remote, local
	if o.upload {
		sourceFS, targetFS = local, remote
	}
	target := targetFS.clean(o.target.path)
	targetDir, err := o.destinationIsDir(target, targetFS.lstat)
	if err != nil {
		return err
	}

	// Enumerate the sources before creating output, including when both sides
	// of the SSH connection can see the destination through the same filesystem.
	var entries []recursiveCopyEntry
	destinations := make(map[string]bool)
	for _, operand := range o.sources {
		source := sourceFS.clean(operand.path)
		destination := target
		if targetDir && !exactTarget {
			name := sourceFS.base(source)
			if name != "." {
				if err := files.ValidateName(name); err != nil {
					return err
				}
				destination = targetFS.join(target, name)
			}
		}
		err := sourceFS.walk(source, func(rel string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if rel != "." {
				if err := files.ValidateName(entry.Name()); err != nil {
					return err
				}
			}
			from, to := sourceFS.join(source, rel), targetFS.join(destination, rel)
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("symbolic link: %s", cleanText(from))
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return fmt.Errorf("%s: recursive copy supports only directories and ordinary files", cleanText(from))
			}
			if destinations[to] {
				return fmt.Errorf("duplicate destination: %s", cleanText(to))
			}
			destinations[to] = true
			file := fileCopy{source: from, target: to}
			if !o.upload && !entry.IsDir() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				file.downloadSize = info.Size()
			}
			entries = append(entries, recursiveCopyEntry{fileCopy: file, directory: entry.IsDir()})
			return nil
		})
		if err != nil {
			return fmt.Errorf("prepare copy of %s: %w", cleanText(source), err)
		}
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := entry.copy(ctx, client, targetFS, o.upload); err != nil {
			return fmt.Errorf("copy %s to %s: %w", cleanText(entry.source), cleanText(entry.target), err)
		}
	}
	termui.Log(os.Stderr, "", termui.Success, "Copied %d entries to %s\n", len(entries), cleanText(o.target.path))
	return nil
}

func (side copyFilesystem) walk(source string, visit fs.WalkDirFunc) error {
	// WalkDir follows a symlink at its root. Inspect the operand itself first.
	info, err := side.lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return visit(".", fs.FileInfoToDirEntry(info), nil)
	}
	return fs.WalkDir(side.dirFS(source), ".", visit)
}

func (entry recursiveCopyEntry) copy(ctx context.Context, client *sftp.Client, targetFS copyFilesystem, upload bool) error {
	if entry.directory {
		info, err := targetFS.lstat(entry.target)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return targetFS.mkdir(entry.target, 0755)
		case err != nil:
			return err
		case !info.IsDir():
			return fmt.Errorf("destination must be a directory")
		default:
			return nil
		}
	}
	if err := files.CheckCopyTarget(entry.target, targetFS.lstat); err != nil {
		return err
	}
	if upload {
		if err := files.ValidateName(path.Base(entry.target)); err != nil {
			return err
		}
		source, err := files.OpenSource(entry.source)
		if err != nil {
			return err
		}
		defer source.Close()
		entry.opened = source
	}
	return entry.transfer(ctx, client, nil)
}

func checkCopyDirectory(name string, allowMissing bool) error {
	info, err := os.Lstat(name)
	if allowMissing && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("must be a directory: %s", cleanText(name))
	}
	return nil
}
