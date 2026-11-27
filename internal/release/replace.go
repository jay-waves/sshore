package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/sys"
)

type Replacement struct {
	InstalledVersion string
	target, staged   string
	previous         os.FileInfo
}

// PrepareReplacement validates the target and stages a verified executable
// before the caller stops any tasks.
func PrepareReplacement(target, version string) (Replacement, error) {
	if !filepath.IsAbs(target) {
		return Replacement{}, fmt.Errorf("installation target must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return Replacement{}, err
	}
	if err := sys.ValidateInstallTarget(target); err != nil {
		return Replacement{}, err
	}
	previous, err := os.Lstat(target)
	if err != nil && !os.IsNotExist(err) {
		return Replacement{}, err
	}
	var installed string
	if previous != nil {
		if !previous.Mode().IsRegular() {
			return Replacement{}, fmt.Errorf("installation target must be a regular executable: %s", target)
		}
		installed, err = executableVersion(target)
		order, known := compare(installed, version)
		if err != nil || !known {
			return Replacement{}, fmt.Errorf("unknown installed release; update manually")
		}
		if order > 0 {
			return Replacement{}, fmt.Errorf("installed sshore %s is newer than %s", installed, version)
		}
	}
	staged, err := stageReplacement(target, version)
	if err != nil {
		return Replacement{}, err
	}
	r := Replacement{InstalledVersion: installed, target: target, staged: staged, previous: previous}
	if err := verifyTarget(target, previous); err != nil {
		r.Discard()
		return Replacement{}, err
	}
	return r, nil
}

func (r Replacement) Publish() error {
	if err := verifyTarget(r.target, r.previous); err != nil {
		return err
	}
	if err := os.Rename(r.staged, r.target); err != nil {
		return fmt.Errorf("replace installed executable: %w", err)
	}
	return nil
}

func (r Replacement) Discard() { _ = os.Remove(r.staged) }

func verifyTarget(path string, previous os.FileInfo) error {
	current, err := os.Lstat(path)
	if os.IsNotExist(err) && previous == nil {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous == nil || current == nil || !os.SameFile(previous, current) {
		return fmt.Errorf("installation target changed; restart installation")
	}
	return nil
}

func stageReplacement(target, version string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer source.Close()
	staged, err := sys.StageSiblingFile(target, ".sshore-update-*.exe", func(file *os.File) error {
		if _, err := io.Copy(file, source); err != nil {
			return err
		}
		return file.Chmod(0755)
	})
	if err != nil {
		return "", err
	}
	actual, err := executableVersion(staged)
	if err == nil && actual == version {
		return staged, nil
	}
	_ = os.Remove(staged)
	if err != nil {
		return "", fmt.Errorf("prepared executable verification failed: %w", err)
	}
	return "", fmt.Errorf("prepared executable reports sshore %q, expected %q", actual, version)
}
