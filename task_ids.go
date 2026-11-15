package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Keys contain immutable task identities, not mutable display names. IDs are
// never recycled, so a command copied from an older list cannot select a new task.
type taskIDIndex struct {
	Next uint64
	IDs  map[string]uint64
}

func assignTaskIDs(keys []string) (map[string]uint64, error) {
	dir, err := stateDir("tasks")
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "ids.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = privateState(lockPath, false); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = lockResourceState(f)
		if err == nil {
			break
		}
		if !resourceLockBusy(err) || time.Now().After(deadline) {
			return nil, fmt.Errorf("task IDs unavailable: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	path := filepath.Join(dir, "ids.json")
	index := taskIDIndex{Next: 1, IDs: map[string]uint64{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &index); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if index.Next == 0 || index.IDs == nil {
		return nil, fmt.Errorf("invalid task ID index")
	}
	seen := map[uint64]bool{}
	for _, id := range index.IDs {
		if id == 0 || id >= index.Next || seen[id] {
			return nil, fmt.Errorf("invalid task ID index")
		}
		seen[id] = true
	}
	result := make(map[string]uint64, len(keys))
	changed := false
	for _, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("missing task identity")
		}
		id := index.IDs[key]
		if id == 0 {
			if index.Next == ^uint64(0) {
				return nil, fmt.Errorf("task IDs exhausted")
			}
			id = index.Next
			index.Next++
			index.IDs[key] = id
			changed = true
		}
		result[key] = id
	}
	if changed {
		if err = writeStateJSON(path, index); err != nil {
			return nil, err
		}
	}
	return result, nil
}
