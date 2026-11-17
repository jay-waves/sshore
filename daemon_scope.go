//go:build !sshoretest

package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

// The daemon owner is account-wide, independent of a client's runtime or cache.
func daemonLocation() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(account.HomeDir, ".sshore", "daemon"), nil
}

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

func daemonStateRootAt(dir, root string, explicit bool) (string, error) {
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	defer owner.Close()
	if err := privateStatePath(owner.Name(), false); err != nil {
		return "", err
	}
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
