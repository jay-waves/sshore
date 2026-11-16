package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if err = privateStatePath(lockPath, false); err != nil {
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

// A bare number denotes a task ID; named remote destinations remain available
// with the normal HOST NAME form. Resolution never falls back to a name.
func runTaskID(args []string) (int, bool, error) {
	id, handled, err := parseTaskID(args)
	if !handled {
		return 0, false, nil
	}
	if err != nil {
		return 2, true, err
	}
	rows, err := localShells()
	if err != nil {
		return 1, true, err
	}
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		if args[0] == "review" {
			return 2, true, usage("task %d is a shell, not a review", id)
		}
		o := options{local: true, host: "local", session: row.Name, command: args[0], taskIdentity: row.identity}
		if args[0] == "rename" {
			o.newName = args[2]
		}
		code, err := runLocalCommand(o)
		return code, true, err
	}
	if code, found, err := runResourceTaskID(id, args); found {
		return code, true, err
	}
	if code, found, err := runRemoteShellID(id, args); found {
		return code, true, err
	}
	peers, err := collectPeerViews()
	if err != nil {
		return 1, true, err
	}
	inv := inventory{Peers: peers}
	if err := numberInventoryPeers(&inv); err != nil {
		return 1, true, err
	}
	for _, p := range inv.Peers {
		if p.TaskID == id {
			return 2, true, usage("task %d is read-only here; manage it on %s", id, cleanText(p.Manager))
		}
	}
	return 1, true, fmt.Errorf("task %d is unavailable; refresh with sshore list", id)
}

func resourceIdentity(r resourceRecord) string {
	if r.Token == "" {
		return ""
	}
	return fmt.Sprintf("%s:%x", r.Kind, sha256.Sum256([]byte(r.Token)))
}

func numberInventoryResources(inv *inventory) error {
	keys := []string{}
	for _, r := range inv.Resources {
		if r.Identity != "" {
			keys = append(keys, r.Identity)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Resources {
		inv.Resources[i].TaskID = ids[inv.Resources[i].Identity]
	}
	sort.SliceStable(inv.Resources, func(i, j int) bool { return inv.Resources[i].TaskID < inv.Resources[j].TaskID })
	return nil
}

func runResourceTaskID(id uint64, args []string) (int, bool, error) {
	records, err := collectLocalResources("")
	if err != nil {
		return 1, true, err
	}
	keys := []string{}
	for _, r := range records {
		if key := resourceIdentity(r); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return 1, true, err
	}
	for _, r := range records {
		key := resourceIdentity(r)
		if ids[key] != id {
			continue
		}
		if r.Kind == "review" {
			switch args[0] {
			case "review":
				code, err := runReview([]string{r.Token})
				return code, true, err
			case "cancel":
				code, err := runCancel([]string{"review/" + r.Token})
				return code, true, err
			}
		} else if args[0] == "cancel" {
			return 0, true, stopResource(r.Kind, r.Name, key)
		}
		return 2, true, usage("%s is not supported for task %d (%s)", args[0], id, r.Kind)
	}
	return 0, false, nil
}

func remoteShellIdentity(host, instance string) string {
	if instance == "" {
		return ""
	}
	return "remote:" + host + ":" + instance
}

func numberInventoryShells(inv *inventory) error {
	keys := []string{}
	for _, s := range inv.Shells {
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		if key := remoteShellIdentity(host, s.Instance); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Shells {
		s := &inv.Shells[i]
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		s.TaskID = ids[remoteShellIdentity(host, s.Instance)]
	}
	return nil
}

func runRemoteShellID(id uint64, args []string) (int, bool, error) {
	rows, err := readShellSnapshots()
	if err != nil {
		return 1, true, err
	}
	keys := []string{}
	for _, s := range rows {
		if key := remoteShellIdentity(s.Host, s.Instance); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return 1, true, err
	}
	for _, s := range rows {
		if s.State == "renamed" {
			continue
		}
		if ids[remoteShellIdentity(s.Host, s.Instance)] != id {
			continue
		}
		if endedShellState(s.State) {
			if args[0] != "cancel" {
				return 1, true, fmt.Errorf("task %d has ended; use sshore cancel %d to clear its history", id, id)
			}
			cleared, err := forgetEndedShell(s.Host, s.Name, s.Instance)
			if err == nil && !cleared {
				err = fmt.Errorf("shell record changed; refresh with sshore list")
			}
			return 0, true, err
		}
		if args[0] == "review" {
			return 2, true, fmt.Errorf("task %d is a shell, not a review", id)
		}
		o := options{host: s.Host, session: s.Name, instance: s.Instance, command: "attach", server: "sshore"}
		if err := resolveRecentAttach(&o); err != nil {
			return 1, true, err
		}
		o.command = args[0]
		if o.command == "rename" {
			o.newName = args[2]
		}
		code, err := runClient(o)
		return code, true, err
	}
	return 0, false, nil
}
