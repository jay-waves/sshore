package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

func daemonDirectory() (string, error) {
	dir, err := daemonLocation()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := privateStatePath(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}

func daemonStateRoot(root string, explicit bool) (string, error) {
	dir, err := daemonLocation()
	if err != nil {
		return "", err
	}
	return daemonStateRootAt(dir, root, explicit)
}

// Open the existing account lock without creating state or taking ownership.
func openDaemonOwner(dir string) (*os.File, error) {
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := privateStatePath(owner.Name(), false); err != nil {
		owner.Close()
		return nil, fmt.Errorf("validate account owner lock: %w", err)
	}
	return owner, nil
}

func daemonStateRootAt(dir, root string, explicit bool) (string, error) {
	owner, err := openDaemonOwner(dir)
	if os.IsNotExist(err) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	defer owner.Close()
	err = lockResourceState(owner)
	if err == nil {
		return root, nil
	}
	if !resourceLockBusy(err) {
		return "", err
	}
	var record localShellRecord
	for until := time.Now().Add(time.Second); ; {
		record, err = readLocalRecord(filepath.Join(dir, "daemon.json"), "daemon")
		if !os.IsNotExist(err) || time.Now().After(until) {
			break
		}
		// A creator publishes the locator after taking ownership; an idle daemon
		// removes it just before releasing ownership. Neither gap changes scope.
		if lockErr := lockResourceState(owner); lockErr == nil {
			return root, nil
		} else if !resourceLockBusy(lockErr) {
			return "", lockErr
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return "", fmt.Errorf("account daemon state unavailable: %w", err)
	}
	if !filepath.IsAbs(record.Directory) {
		return "", fmt.Errorf("invalid account daemon state root")
	}
	same := filepath.Clean(root) == filepath.Clean(record.Directory)
	if !same {
		a, aErr := os.Stat(root)
		b, bErr := os.Stat(record.Directory)
		same = aErr == nil && bErr == nil && os.SameFile(a, b)
	}
	if explicit && !same {
		return "", fmt.Errorf("account daemon uses %s; SSHORE_STATE_DIR cannot select another root while it is running", record.Directory)
	}
	return record.Directory, nil
}

// Daemon ownership follows the authenticated OS account, independently of runtime state.
var daemonLocation = func() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(account.HomeDir, ".sshore", "daemon"), nil
}

var resetCachePath = func() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sshore"), nil
}
