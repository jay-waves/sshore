package sys

import (
	"errors"
	"fmt"
	"os"

	"github.com/gofrs/flock"
)

var ErrFileLockBusy = errors.New("file lock is busy")

// FileLock keeps the caller's validated file open while flock manages the lock.
// The path must keep referring to that file across acquisition attempts.
type FileLock struct {
	file *os.File
	lock *flock.Flock
}

// NewFileLock takes ownership of an already opened and validated lock file.
func NewFileLock(file *os.File) *FileLock {
	return &FileLock{file: file, lock: flock.New(file.Name(), flock.SetFlag(os.O_RDWR))}
}

func (l *FileLock) Path() string { return l.lock.Path() }

func (l *FileLock) Close() error { return errors.Join(l.lock.Close(), l.file.Close()) }

// TryLock takes an exclusive lock without waiting.
func (l *FileLock) TryLock() error {
	return l.finishLock(l.lock.TryLock())
}

// TryReadLock allows other readers to acquire the same lock.
func (l *FileLock) TryReadLock() error {
	return l.finishLock(l.lock.TryRLock())
}

func (l *FileLock) finishLock(locked bool, err error) error {
	if err != nil {
		return err
	}
	if !locked {
		return ErrFileLockBusy
	}
	if err := l.checkFile(); err != nil {
		return errors.Join(err, l.lock.Close())
	}
	return nil
}

func (l *FileLock) checkFile() error {
	original, err := l.file.Stat()
	if err != nil {
		return err
	}
	locked, err := l.lock.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(original, locked) {
		return fmt.Errorf("lock file changed: %s", l.Path())
	}
	return nil
}
