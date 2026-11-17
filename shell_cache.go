package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
)

const shellHistoryRetention = 7 * 24 * time.Hour
const maxShellSnapshot = 128 * 1024

func endedShellState(state string) bool {
	return state == "exited" || state == "renamed"
}

type shellSnapshot struct {
	Connection  *recentConnection `json:"connection,omitempty"`
	MachineID   string            `json:"machine_id,omitempty"`
	Hostname    string            `json:"hostname,omitempty"`
	Forgotten   bool              `json:"forgotten,omitempty"`
	Temporary   bool              `json:"temporary,omitempty"`
	Instance    string            `json:"instance,omitempty"`
	Host        string            `json:"host"`
	Name        string            `json:"name"`
	State       string            `json:"state"`
	ConfirmedAt time.Time         `json:"confirmed_at"`
	Unconfirmed bool              `json:"unconfirmed,omitempty"`
	Active      bool              `json:"active,omitempty"`
}

func shellCachePath(host, name string, scope ...string) (string, error) {
	if validSSHDestination(host) != nil || validSession(name) != nil {
		return "", fmt.Errorf("invalid shell identity")
	}
	dir, err := statePath("shells")
	if err != nil {
		return "", err
	}
	key := host + "\x00" + name
	if len(scope) > 0 {
		key += "\x00" + scope[0]
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}
func (s shellSnapshot) cachePath() (string, error) {
	if s.Connection != nil && s.Connection.Parent != nil {
		return shellCachePath(s.Host, s.Name, s.Instance)
	}
	return shellCachePath(s.Host, s.Name)
}

func saveShellSnapshot(s shellSnapshot) error {
	_, err := metadataRPC(metadataRequest{Action: "shell_save", Snapshot: &s})
	return err
}
func saveShellSnapshotLocked(s shellSnapshot) error {
	path, err := s.cachePath()
	if err != nil {
		return err
	}
	s.Active = false
	if s.Connection != nil {
		connection := s.Connection.clone()
		s.Connection = &connection
	}
	accountActivity.Load().metadata.snapshots[path] = s
	var treeErr error
	if s.State == "exited" {
		treeErr = recordRemoteShellTreeState(s)
	}
	return errors.Join(treeErr, writeStateJSON(path, s))
}
func readShellSnapshots() ([]shellSnapshot, error) {
	r, err := metadataRPC(metadataRequest{Action: "shell_get"})
	return r.Snapshots, err
}
func readShellSnapshotsLocked() ([]shellSnapshot, error) {
	reply := accountActivity.Load().request(activityRequest{Kind: "list"})
	active := map[string]bool{}
	for _, key := range reply.Active {
		active[key] = true
	}
	var rows []shellSnapshot
	for path, s := range accountActivity.Load().metadata.snapshots {
		if s.Connection != nil {
			if s.Connection.Host != s.Host || s.Connection.Parent != nil && (s.Connection.Session != s.Name || s.Connection.Instance != s.Instance) {
				return nil, fmt.Errorf("invalid shell connection host")
			}
			if err := validateRecentConnection(*s.Connection); err != nil {
				return nil, err
			}
		}
		if s.Instance != "" && !endedShellState(s.State) {
			for _, n := range shellTreeFor(remoteTreeIdentity(s.Instance)) {
				if n.Identity == remoteTreeIdentity(s.Instance) && n.State == "exited" {
					// Project the original evidence; reading history is not a new confirmation.
					s.State, s.Unconfirmed, s.ConfirmedAt = n.State, false, n.CheckedAt
					break
				}
			}
		}
		s.Active = active[path]
		if !s.Active && (endedShellState(s.State) || s.Unconfirmed) && !s.ConfirmedAt.IsZero() && time.Since(s.ConfirmedAt) > shellHistoryRetention {
			delete(accountActivity.Load().metadata.snapshots, path)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}
		if s.Connection != nil {
			connection := s.Connection.clone()
			s.Connection = &connection
		}
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Host != rows[j].Host {
			return rows[i].Host < rows[j].Host
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

type shellConnection struct {
	snapshot shellSnapshot
	stop     func()
}

func trackShellConnection(o options) (*shellConnection, error) {
	if o.local {
		return nil, nil
	}
	connection, err := connectionFromOptions(o)
	if err != nil {
		return nil, err
	}
	snapshot := shellSnapshot{Connection: &connection, Temporary: o.temporary, Instance: o.instance, Host: o.host, Name: o.session, State: "attached", ConfirmedAt: time.Now()}
	path, err := snapshot.cachePath()
	if err != nil {
		return nil, err
	}
	stop, err := leaseActivity(activityRequest{Kind: "shell", Key: path}, nil)
	if err != nil {
		return nil, err
	}
	s := &shellConnection{stop: stop, snapshot: snapshot}
	if err = saveShellSnapshot(s.snapshot); err != nil {
		return s, err
	}
	return s, nil
}
func (s *shellConnection) confirm(name, state string) {
	if s == nil {
		return
	}
	if validSession(name) == nil && name != s.snapshot.Name {
		s.snapshot.State = "renamed"
		s.snapshot.ConfirmedAt = time.Now()
		if err := saveShellSnapshot(s.snapshot); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Shell state not saved: %s\n", cleanText(err.Error()))
		}
		s.snapshot.Name = name
		if s.snapshot.Connection != nil {
			s.snapshot.Connection.Session = name
		}
	}
	s.snapshot.State, s.snapshot.ConfirmedAt, s.snapshot.Unconfirmed = state, time.Now(), false
	if err := saveShellSnapshot(s.snapshot); err != nil {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Shell state not saved: %s\n", cleanText(err.Error()))
	}
}
func (s *shellConnection) close() {
	if s == nil {
		return
	}
	defer s.stop()
	if s.snapshot.State == "attached" {
		s.snapshot.Unconfirmed = true
		if err := saveShellSnapshot(s.snapshot); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Shell state not saved: %s\n", cleanText(err.Error()))
		}
	}
}

func cacheRemoteShells(host string, inv inventory, connection ...options) error {
	request := metadataRequest{Action: "shell_cache", Host: host, Inventory: &inv}
	if len(connection) > 0 {
		route, err := connectionFromOptions(connection[0])
		if err != nil {
			return err
		}
		request.Connection = &route
	}
	_, err := metadataRPC(request)
	return err
}
func cacheRemoteShellsLocked(host string, inv inventory, connection ...recentConnection) error {
	seen := map[string]bool{}
	names := map[string]string{}
	now := time.Now()
	for _, shell := range inv.Shells {
		if shell.Instance != "" {
			names[shell.Instance] = shell.Name
		}
		seen[shell.Name] = true
		snapshot := shellSnapshot{Temporary: shell.Temporary, Instance: shell.Instance, Host: host, MachineID: inv.MachineID, Hostname: inv.Hostname, Name: shell.Name, State: shell.State, ConfirmedAt: now}
		if len(connection) > 0 {
			route := connection[0]
			if route.Parent != nil && shell.Instance != route.Instance {
				route.RouteTarget = true
			}
			route.Host, route.Session, route.Instance = host, shell.Name, shell.Instance
			snapshot.Connection = &route
		}
		if err := saveShellSnapshotLocked(snapshot); err != nil {
			return err
		}
		// A fresh remote observation can supersede an earlier absence report.
		if shell.Instance != "" && !endedShellState(shell.State) {
			for _, n := range shellTreeFor(remoteTreeIdentity(shell.Instance)) {
				if n.Identity == remoteTreeIdentity(shell.Instance) && endedShellState(n.State) {
					if err := recordRemoteShellTreeState(snapshot); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	// Partial results cannot establish absence from this account's inventory.
	if len(inv.Errors) != 0 {
		return nil
	}
	previous, err := readShellSnapshotsLocked()
	if err != nil {
		return err
	}
	for _, old := range previous {
		sameScope := old.Connection == nil || old.Connection.Parent == nil
		missing := !seen[old.Name]
		if len(connection) > 0 && connection[0].Parent != nil {
			// The same alias on another parent does not describe this inventory.
			sameScope = old.Connection != nil && old.Connection.Parent != nil && old.Connection.RouteToken == connection[0].RouteToken && old.Connection.Parent.Instance == connection[0].Parent.Instance
			missing = names[old.Instance] != old.Name
		}
		if old.Host == host && sameScope && missing && !endedShellState(old.State) {
			if names[old.Instance] != "" {
				old.State, old.ConfirmedAt, old.Unconfirmed = "renamed", now, false
			} else if old.Active || old.Unconfirmed {
				continue
			} else {
				old.Unconfirmed = true
			}
			if err := saveShellSnapshotLocked(old); err != nil {
				return err
			}
		}
	}
	return nil
}

// renameShellSnapshot retains the last actual confirmation and leaves live locks
// with their owning clients. It never creates evidence for an uncached shell.
func renameShellSnapshot(host, old, name string) error {
	_, err := metadataRPC(metadataRequest{Action: "shell_rename", Host: host, Key: old, Name: name})
	return err
}
func renameShellSnapshotLocked(host, old, name string) error {
	rows, err := readShellSnapshotsLocked()
	if err != nil {
		return err
	}
	var snapshot shellSnapshot
	found := false
	for _, row := range rows {
		if row.Host != host || row.Name != old || row.State == "renamed" {
			continue
		}
		if found {
			return fmt.Errorf("ambiguous shell route; select the shell by task ID")
		}
		snapshot, found = row, true
	}
	if !found {
		return nil
	}
	renamed := snapshot
	renamed.Name = name
	if snapshot.Connection != nil {
		connection := *snapshot.Connection
		connection.Session = name
		renamed.Connection = &connection
	}
	if err = saveShellSnapshotLocked(renamed); err != nil {
		return err
	}
	snapshot.State = "renamed"
	return saveShellSnapshotLocked(snapshot)
}

// Cleanup is local and instance-bound; a replacement with the same name is not
// a cleanup target. Keep a short-lived tombstone for repeated cancellation.
func forgetEndedShell(host, name, instance string) (bool, error) {
	r, err := metadataRPC(metadataRequest{Action: "shell_forget", Host: host, Name: name, Instance: instance})
	return r.Found, err
}
func forgetEndedShellLocal(host, name, instance string) (bool, error) {
	rows, err := readShellSnapshotsLocked()
	if err != nil {
		return false, err
	}
	for _, s := range rows {
		if s.Host != host || s.Name != name || (instance != "" && s.Instance != instance) {
			continue
		}
		if s.State != "exited" {
			return false, nil
		}
		s.Forgotten = true
		return true, saveShellSnapshotLocked(s)
	}
	return false, nil
}

func confirmKilledShell(host, name, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "shell_cancel", Host: host, Name: name, Instance: instance})
	return err
}
func confirmKilledShellLocal(host, name, instance string) error {
	rows, err := readShellSnapshotsLocked()
	if err != nil {
		return err
	}
	for _, s := range rows {
		if s.Host == host && s.Name == name && s.Instance == instance {
			s.State, s.Unconfirmed, s.ConfirmedAt = "exited", false, time.Now()
			return saveShellSnapshotLocked(s)
		}
	}
	if instance != "" {
		return recordRemoteShellTreeState(shellSnapshot{Host: host, Name: name, Instance: instance, State: "exited", ConfirmedAt: time.Now()})
	}
	return nil
}
