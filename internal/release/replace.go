package release

import (
	"fmt"
	"io"
	"os"

	"github.com/jay-waves/sshore/internal/sys"
)

func VerifyTarget(path string, previous os.FileInfo) error {
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

func StageReplacement(target, version string) (string, error) {
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
	actual, err := ExecutableVersion(staged)
	if err == nil && actual == version {
		return staged, nil
	}
	_ = os.Remove(staged)
	if err != nil {
		return "", fmt.Errorf("prepared executable verification failed: %w", err)
	}
	return "", fmt.Errorf("prepared executable reports sshore %q, expected %q", actual, version)
}
