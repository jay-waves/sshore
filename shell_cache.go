package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	statefs "github.com/jay-waves/sshore/internal/state"
	"github.com/jay-waves/sshore/internal/termui"
)

type shellSnapshot struct {
	Temporary   bool      `json:"temporary,omitempty"`
	Instance    string    `json:"instance,omitempty"`
	Host        string    `json:"host"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	ConfirmedAt time.Time `json:"confirmed_at"`
	Unconfirmed bool      `json:"unconfirmed,omitempty"`
	Active      bool      `json:"local_connection_active,omitempty"`
}

func shellCachePath(host, name string) (string, error) {
	if validSSHDestination(host) != nil || validSession(name) != nil {
		return "", fmt.Errorf("invalid shell identity")
	}
	dir, err := statefs.Dir("shells")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(host + "\x00" + name))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}
func saveShellSnapshot(s shellSnapshot) error {
	path, err := shellCachePath(s.Host, s.Name)
	if err != nil {
		return err
	}
	s.Active = false
	return statefs.WriteJSON(path, s)
}
func readShellSnapshots() ([]shellSnapshot, error) {
	dir, err := statefs.Dir("shells")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var rows []shellSnapshot
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := statefs.Private(path, false); err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		var s shellSnapshot
		err = json.NewDecoder(io.LimitReader(f, 8192)).Decode(&s)
		f.Close()
		if err != nil {
			return nil, err
		}
		expected, err := shellCachePath(s.Host, s.Name)
		if err != nil || path != expected {
			return nil, fmt.Errorf("invalid shell cache record")
		}
		live, err := os.OpenFile(path+".live", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		err = lockResourceState(live)
		live.Close()
		s.Active = resourceLockBusy(err)
		if err != nil && !s.Active {
			return nil, err
		}
		rows = append(rows, s)
	}
	recent, err := readRecentConnections()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Host+"\x00"+r.Name] = true
	}
	for _, r := range recent {
		if !seen[r.Host+"\x00"+r.Session] {
			rows = append(rows, shellSnapshot{Host: r.Host, Name: r.Session, State: "unknown", Unconfirmed: true})
		}
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
	lock     *os.File
	final    bool
}

func trackShellConnection(o options) (*shellConnection, error) {
	if o.plain || o.local {
		return nil, nil
	}
	path, err := shellCachePath(o.host, o.session)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".live", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = statefs.Private(path+".live", false); err == nil {
		err = lockResourceState(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	s := &shellConnection{lock: f, snapshot: shellSnapshot{Temporary: o.temporary, Instance: o.instance, Host: o.host, Name: o.session, State: "attached", ConfirmedAt: time.Now()}}
	if err = saveShellSnapshot(s.snapshot); err != nil {
		f.Close()
		return nil, err
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
	}
	s.snapshot.State, s.snapshot.ConfirmedAt, s.snapshot.Unconfirmed = state, time.Now(), false
	s.final = true
	if err := saveShellSnapshot(s.snapshot); err != nil {
		termui.Log(os.Stderr, termui.Warning, "[sshore] Shell state not saved: %s\n", cleanText(err.Error()))
	}
}
func (s *shellConnection) close() {
	if s == nil {
		return
	}
	defer s.lock.Close()
	if !s.final {
		s.snapshot.Unconfirmed = true
		if err := saveShellSnapshot(s.snapshot); err != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Shell state not saved: %s\n", cleanText(err.Error()))
		}
	}
}

func cacheRemoteShells(host string, inv inventory) error {
	seen := map[string]bool{}
	now := time.Now()
	for _, shell := range inv.Shells {
		seen[shell.Name] = true
		if err := saveShellSnapshot(shellSnapshot{Temporary: shell.Temporary, Instance: shell.Instance, Host: host, Name: shell.Name, State: shell.State, ConfirmedAt: now}); err != nil {
			return err
		}
	}
	// An incomplete inventory cannot establish that a missing shell ended.
	if len(inv.Errors) != 0 {
		return nil
	}
	previous, err := readShellSnapshots()
	if err != nil {
		return err
	}
	for _, old := range previous {
		if old.Host == host && !seen[old.Name] {
			old.State, old.ConfirmedAt, old.Unconfirmed = "not-listed", now, false
			if err := saveShellSnapshot(old); err != nil {
				return err
			}
		}
	}
	return nil
}

// renameShellSnapshot retains the last actual confirmation and leaves live locks
// with their owning clients. It never creates evidence for an uncached shell.
func renameShellSnapshot(host, old, name string) error {
	oldPath, err := shellCachePath(host, old)
	if err != nil {
		return err
	}
	if err = statefs.Private(oldPath, false); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	file, err := os.Open(oldPath)
	if err != nil {
		return err
	}
	var snapshot shellSnapshot
	err = json.NewDecoder(io.LimitReader(file, 8192)).Decode(&snapshot)
	file.Close()
	if err != nil {
		return err
	}
	if snapshot.Host != host || snapshot.Name != old {
		return fmt.Errorf("invalid shell cache record")
	}
	renamed := snapshot
	renamed.Name = name
	if err = saveShellSnapshot(renamed); err != nil {
		return err
	}
	snapshot.State = "renamed"
	return saveShellSnapshot(snapshot)
}
