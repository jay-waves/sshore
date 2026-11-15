package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errObjectNameInUse = errors.New("name is already in use")

type resourceNameReservation struct {
	name           string
	startup, owner *os.File
}

func (r *resourceNameReservation) Close() {
	if r.owner != nil {
		r.owner.Close()
	}
	r.startup.Close()
}

// The startup lock bridges the handoff from the creator to the worker's lifetime
// lock. Lock files stay in place so concurrent creators always lock the same inode.
func reserveResourceName(kind, requested string) (*resourceNameReservation, error) {
	return reserveResourceNameExcept(kind, requested, "")
}

func reserveResourceNameExcept(kind, requested, reviewID string) (*resourceNameReservation, error) {
	names := automaticSessionNames()
	if requested != "" {
		if err := validSession(requested); err != nil {
			return nil, err
		}
		names = []string{requested}
	}
	dir, err := stateDir("names")
	if err != nil {
		return nil, err
	}
	occupied, err := occupiedObjectNames(reviewID)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if occupied[name] {
			if requested != "" {
				return nil, fmt.Errorf("%s: %w", name, errObjectNameInUse)
			}
			continue
		}
		var held []*os.File
		var lockErr error
		for _, suffix := range []string{".startup.lock", ".lock"} {
			path := filepath.Join(dir, name+suffix)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				for _, h := range held {
					h.Close()
				}
				return nil, err
			}
			held = append(held, f)
			if err = privateState(path, false); err != nil {
				for _, h := range held {
					h.Close()
				}
				return nil, err
			}
			if lockErr = lockResourceState(f); lockErr != nil {
				break
			}
		}
		if lockErr == nil {
			current, err := occupiedObjectNames(reviewID)
			if err != nil || current[name] {
				for _, f := range held {
					f.Close()
				}
				if err != nil {
					return nil, err
				}
				if requested != "" {
					return nil, fmt.Errorf("%s: %w", name, errObjectNameInUse)
				}
				continue
			}
			return &resourceNameReservation{name: name, startup: held[0], owner: held[1]}, nil
		}
		for _, f := range held {
			f.Close()
		}
		if !resourceLockBusy(lockErr) {
			return nil, lockErr
		}
		if requested != "" {
			return nil, fmt.Errorf("%s/%s: %w", kind, name, errObjectNameInUse)
		}
	}
	return nil, fmt.Errorf("all automatic %s names are in use; choose a name with --name", kind)
}

func lockObjectName(name string) (*os.File, error) {
	if err := validSession(name); err != nil {
		return nil, err
	}
	dir, err := stateDir("names")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateState(path, false); err == nil {
		err = lockResourceState(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func occupiedObjectNames(exceptReview string) (map[string]bool, error) {
	names, err := occupiedShellNames()
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"mount", "forward"} {
		dir, err := resourceStateDir(kind)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".lock") || strings.HasSuffix(entry.Name(), ".startup.lock") {
				continue
			}
			f, err := os.OpenFile(filepath.Join(dir, entry.Name()), os.O_RDWR, 0600)
			if err != nil {
				return nil, err
			}
			err = lockResourceState(f)
			f.Close()
			if resourceLockBusy(err) {
				names[strings.TrimSuffix(entry.Name(), ".lock")] = true
			} else if err != nil {
				return nil, err
			}
		}
	}
	dir, err := approvalDir()
	if err != nil {
		return nil, err
	}
	pending, err := pendingApprovals(dir)
	if err != nil {
		return nil, err
	}
	for _, r := range pending {
		if r.ID != exceptReview {
			names[r.DisplayName()] = true
		}
	}
	return names, nil
}
