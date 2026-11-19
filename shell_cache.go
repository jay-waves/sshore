package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func (s shellSnapshot) cachePath(dir string) (string, error) {
	if validSSHDestination(s.Host) != nil || validSession(s.Name) != nil {
		return "", fmt.Errorf("invalid shell identity")
	}
	key := s.Host + "\x00" + s.Name
	if s.Connection != nil && s.Connection.Parent != nil {
		key += "\x00" + s.Instance
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

func (s shellSnapshot) validateRoute() error {
	route := s.Connection
	if route == nil {
		return nil
	}
	if route.Host != s.Host || route.Parent != nil && (route.Session != s.Name || route.Instance != s.Instance) {
		return fmt.Errorf("invalid shell connection identity")
	}
	return validateRecentConnection(*route)
}

func saveShellSnapshot(s shellSnapshot) error {
	_, err := metadataRPC(metadataRequest{Action: "shell_save", Snapshot: &s})
	return err
}

// Routes passed here are owned by the daemon and read-only. Internal state
// updates can reuse them, and rows from the same inventory can share ancestors.
func (a *daemonActivity) saveShellSnapshotLocked(s shellSnapshot) error {
	if err := s.validateRoute(); err != nil {
		return err
	}
	path, err := s.cachePath(a.metadata.shellRoot)
	if err != nil {
		return err
	}
	s.Active = false
	writeErr := writeBoundedStateJSON(path, s, maxShellSnapshot)
	if writeErr == nil {
		a.metadata.snapshots[path] = s
	}
	var treeErr error
	if s.State == "exited" {
		treeErr = a.metadata.recordRemoteShellTreeStateLocal(s)
	}
	return errors.Join(writeErr, treeErr)
}
func readShellSnapshots() ([]shellSnapshot, error) {
	r, err := metadataRPC(metadataRequest{Action: "shell_get"})
	slices.SortFunc(r.Snapshots, func(a, b shellSnapshot) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Name, b.Name))
	})
	return r.Snapshots, err
}

func (a *daemonActivity) readShellSnapshotsLocked(match func(shellSnapshot) bool) ([]shellSnapshot, error) {
	var rows []shellSnapshot
	if err := a.walkShellSnapshotsLocked(func(s shellSnapshot) {
		if match(s) {
			rows = append(rows, s)
		}
	}); err != nil {
		return nil, err
	}
	return rows, nil
}

// Reconcile and prune the full history before returning. The visitor must
// treat projected connection routes as read-only.
func (a *daemonActivity) walkShellSnapshotsLocked(visit func(shellSnapshot)) error {
	active := a.activeShellPaths()
	now := time.Now()
	for path, s := range a.metadata.snapshots {
		if s.Instance != "" && !endedShellState(s.State) {
			id := remoteTreeIdentity(s.Instance)
			if n, ok := a.metadata.treeNode(id); ok && n.State == "exited" {
				// Project the original evidence; reading history is not a new confirmation.
				s.State, s.Unconfirmed, s.ConfirmedAt = n.State, false, n.CheckedAt
			}
		}
		s.Active = active[path]
		if !s.Active && (endedShellState(s.State) || s.Unconfirmed) && !s.ConfirmedAt.IsZero() && now.Sub(s.ConfirmedAt) > shellHistoryRetention {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			delete(a.metadata.snapshots, path)
			continue
		}
		visit(s)
	}
	return nil
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
	dir, err := statePath("shells")
	if err != nil {
		return nil, err
	}
	path, err := snapshot.cachePath(dir)
	if err != nil {
		return nil, err
	}
	stop, err := leaseActivity(activityRequest{Kind: "shell", Key: path})
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
	// Shell history needs only the observations and whether the query was
	// complete. Keep resource rows, peer rows and tree history out of this RPC.
	observed := inventory{MachineID: inv.MachineID, Hostname: inv.Hostname, Shells: inv.Shells, Errors: inv.Errors}
	request := metadataRequest{Action: "shell_cache", Host: host, Inventory: &observed}
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
func (a *daemonActivity) cacheRemoteShellsLocked(host string, inv inventory, connection *recentConnection) error {
	seen := map[string]bool{}
	names := map[string]string{}
	now := time.Now()
	for _, shell := range inv.Shells {
		if shell.Instance != "" {
			names[shell.Instance] = shell.Name
		}
		seen[shell.Name] = true
		snapshot := shellSnapshot{Temporary: shell.Temporary, Instance: shell.Instance, Host: host, MachineID: inv.MachineID, Hostname: inv.Hostname, Name: shell.Name, State: shell.State, ConfirmedAt: now}
		if connection != nil {
			route := *connection
			if route.Parent != nil && shell.Instance != route.Instance {
				route.RouteTarget = true
			}
			route.Host, route.Session, route.Instance = host, shell.Name, shell.Instance
			snapshot.Connection = &route
		}
		if err := a.saveShellSnapshotLocked(snapshot); err != nil {
			return err
		}
		// A fresh remote observation can supersede an earlier absence report.
		if shell.Instance != "" && !endedShellState(shell.State) {
			id := remoteTreeIdentity(shell.Instance)
			if n, ok := a.metadata.treeNode(id); ok && endedShellState(n.State) {
				if err := a.metadata.recordRemoteShellTreeStateLocal(snapshot); err != nil {
					return err
				}
			}
		}
	}
	// Partial results cannot establish absence from this account's inventory.
	if len(inv.Errors) != 0 {
		return nil
	}
	previous, err := a.readShellSnapshotsLocked(func(s shellSnapshot) bool {
		return s.Host == host && !endedShellState(s.State)
	})
	if err != nil {
		return err
	}
	for _, old := range previous {
		sameScope := old.Connection == nil || old.Connection.Parent == nil
		missing := !seen[old.Name]
		if connection != nil && connection.Parent != nil {
			// The same alias on another parent does not describe this inventory.
			sameScope = old.Connection != nil && old.Connection.Parent != nil && old.Connection.RouteToken == connection.RouteToken && old.Connection.Parent.Instance == connection.Parent.Instance
			missing = names[old.Instance] != old.Name
		}
		if sameScope && missing {
			if names[old.Instance] != "" {
				old.State, old.ConfirmedAt, old.Unconfirmed = "renamed", now, false
			} else if old.Active || old.Unconfirmed {
				continue
			} else {
				old.Unconfirmed = true
			}
			if err := a.saveShellSnapshotLocked(old); err != nil {
				return err
			}
		}
	}
	return nil
}

// Retain the last actual confirmation and leave live locks with their owners.
func (a *daemonActivity) renameShellSnapshotLocked(host, old, name, instance string) error {
	rows, err := a.readShellSnapshotsLocked(func(s shellSnapshot) bool {
		return s.Host == host && s.Name == old && s.State != "renamed" && (instance == "" || s.Instance == instance)
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > 1 {
		return fmt.Errorf("ambiguous shell route; select the shell by task ID")
	}
	snapshot := rows[0]
	renamed := snapshot
	renamed.Name = name
	if snapshot.Connection != nil {
		connection := *snapshot.Connection
		connection.Session = name
		renamed.Connection = &connection
	}
	if err = a.saveShellSnapshotLocked(renamed); err != nil {
		return err
	}
	snapshot.State = "renamed"
	return a.saveShellSnapshotLocked(snapshot)
}

// Cleanup is local and instance-bound; a replacement with the same name is not
// a cleanup target. Keep a short-lived tombstone for repeated cancellation.
func forgetEndedShell(host, name, instance string) (bool, error) {
	r, err := metadataRPC(metadataRequest{Action: "shell_forget", Host: host, Name: name, Instance: instance})
	return r.Found, err
}
func (a *daemonActivity) forgetEndedShellLocal(host, name, instance string) (bool, error) {
	rows, err := a.readShellSnapshotsLocked(func(s shellSnapshot) bool {
		return s.Host == host && s.Name == name && (instance == "" || s.Instance == instance)
	})
	if err != nil {
		return false, err
	}
	if len(rows) == 0 || rows[0].State != "exited" {
		return false, nil
	}
	s := rows[0]
	if s.Forgotten {
		return true, nil
	}
	s.Forgotten = true
	return true, a.saveShellSnapshotLocked(s)
}

func confirmKilledShell(host, name, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "shell_cancel", Host: host, Name: name, Instance: instance})
	return err
}
func (a *daemonActivity) confirmKilledShellLocal(host, name, instance string) error {
	rows, err := a.readShellSnapshotsLocked(func(s shellSnapshot) bool {
		return s.Host == host && s.Name == name && s.Instance == instance
	})
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		s := rows[0]
		s.State, s.Unconfirmed, s.ConfirmedAt = "exited", false, time.Now()
		return a.saveShellSnapshotLocked(s)
	}
	if instance != "" {
		return a.metadata.recordRemoteShellTreeStateLocal(shellSnapshot{Host: host, Name: name, Instance: instance, State: "exited", ConfirmedAt: time.Now()})
	}
	return nil
}
