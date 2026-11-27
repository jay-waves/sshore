package main

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
	bolt "go.etcd.io/bbolt"
)

// Participation records only tasks opened through this client account.
type participatingShell struct {
	HostInfo             hostInfo `json:",omitzero"`
	Host, Instance, Name string
	Owner                string
	Shell                string `json:",omitempty"`
	Program              bool   `json:",omitempty"`
	State                string
	PID                  int
	Pinned               bool
	PinUntil             time.Time
	Attachment           string
	ChangedAt            time.Time
}

type participationRefresh struct {
	Shells []shellView
	Since  time.Time
}

func reconcileParticipatingShells(host string, shells []shellView, since time.Time) error {
	refresh := participationRefresh{Shells: shells, Since: since}
	_, err := metadataRPC(metadataRequest{Action: "participation_refresh", Host: host, Participation: &refresh})
	return err
}

func forgetParticipatingShell(host, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_forget", Host: host, Instance: instance})
	return err
}

func observeParticipatingShell(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_observe", Participant: &row})
	return err
}

func setParticipatingShellState(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_state", Participant: &row})
	return err
}

func validateParticipatingShell(row participatingShell) error {
	switch {
	case validSSHDestination(row.Host) != nil:
		return fmt.Errorf("remote shell has an invalid SSH destination")
	case !validTaskUID(row.Instance):
		return fmt.Errorf("remote shell has an invalid task ID")
	case row.Owner != "" && row.Owner != identityFromUID(canonicalTaskUID(row.Owner)):
		return fmt.Errorf("remote shell has an invalid parent task ID")
	case validSession(row.Name) != nil:
		return fmt.Errorf("remote shell has an invalid session name")
	case row.PID <= 0:
		return fmt.Errorf("remote shell has invalid PID %d (expected a positive PID)", row.PID)
	case !validTaskUID(row.Attachment):
		return fmt.Errorf("remote shell has an invalid attachment ID")
	}
	switch row.State {
	case taskActive, taskWaiting, taskDetached, taskUnknown:
		return nil
	default:
		return fmt.Errorf("remote shell has invalid state %.40q (expected %s, %s, %s or %s)", row.State, taskActive, taskWaiting, taskDetached, taskUnknown)
	}
}

const participationLimit = 4096

var participationBucket = []byte("participation")

func participationKey(host, instance string) []byte { return []byte(host + "\x00" + instance) }

// Creating the bucket and importing legacy history share a transaction. Its existence
// records that migration completed, even after all imported rows are forgotten.
func (m *daemonMetadata) loadParticipation(root string) {
	m.participationErr = m.openParticipation(root)
}

func (m *daemonMetadata) openParticipation(root string) error {
	dir, err := prepareStateDir(root, "connections")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "participation.db")
	db, err := bolt.Open(path, 0600, &bolt.Options{
		Timeout:  time.Second,
		OpenFile: openParticipationFile,
	})
	if err != nil {
		return err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(participationBucket) != nil {
			return nil
		}
		var legacy recentHistory[participatingShell]
		if err := legacy.loadAt(root, "participation.json", 1024*1024, participationLimit, validateParticipatingShell); err != nil {
			return err
		}
		bucket, err := tx.CreateBucket(participationBucket)
		if err != nil {
			return err
		}
		for _, row := range legacy.rows {
			if err := writeParticipation(bucket, participationKey(row.Host, row.Instance), row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return err
	}
	m.participationDB = db
	return nil
}

func openParticipationFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	err := sys.PrivateStatePath(path, false)
	if err == nil {
		return os.OpenFile(path, flags&^os.O_CREATE, mode)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, flags|os.O_EXCL, mode)
	if err != nil {
		return nil, err
	}
	if err := sys.PrivateStatePath(path, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (m *daemonMetadata) closeParticipation() {
	if m.participationDB != nil {
		_ = m.participationDB.Close()
	}
}

func (m *daemonMetadata) participatingShells() ([]participatingShell, error) {
	db, err := m.participationDB, m.participationErr
	if db == nil {
		return nil, err
	}
	var rows []participatingShell
	err = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(participationBucket).ForEach(func(_, value []byte) error {
			var row participatingShell
			if err := json.Unmarshal(value, &row); err != nil {
				return err
			}
			if err := validateParticipatingShell(row); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
	})
	return rows, err
}

func writeParticipation(bucket *bolt.Bucket, key []byte, row participatingShell) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

func (m *daemonMetadata) updateParticipation(r metadataRequest) error {
	db, err := m.participationDB, m.participationErr
	if db == nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		now := time.Now()
		bucket := tx.Bucket(participationBucket)
		switch r.Action {
		case "participation_observe", "participation_state":
			if r.Participant == nil {
				return fmt.Errorf("missing participating shell")
			}
			row := *r.Participant
			if err := validateParticipatingShell(row); err != nil {
				return err
			}
			key := participationKey(row.Host, row.Instance)
			var previous participatingShell
			old := bucket.Get(key)
			if old != nil {
				if err := json.Unmarshal(old, &previous); err != nil {
					return err
				}
			}
			if r.Action == "participation_state" {
				if old == nil || previous.Attachment != row.Attachment {
					return nil
				}
				if row.State == taskDetached {
					previous.Name = row.Name
				}
				previous.State, previous.ChangedAt = row.State, now
				previous.Pinned, previous.PinUntil = row.Pinned, row.PinUntil
				row = previous
			} else {
				row.ChangedAt = now
				if old != nil && row.HostInfo == (hostInfo{}) {
					row.HostInfo = previous.HostInfo
				}
			}
			// Only a new observation adds a key. Keep the bounded count scan
			// off the import, refresh and attachment state update paths.
			if old == nil && bucket.Stats().KeyN >= participationLimit {
				return fmt.Errorf("participating shell limit reached")
			}
			return writeParticipation(bucket, key, row)
		case "participation_forget":
			if !validTaskUID(r.Instance) || validSSHDestination(r.Host) != nil {
				return fmt.Errorf("invalid participating shell identity")
			}
			return bucket.Delete(participationKey(r.Host, r.Instance))
		case "participation_refresh":
			if r.Participation == nil || validSSHDestination(r.Host) != nil {
				return fmt.Errorf("invalid participating shell refresh")
			}
			for _, view := range r.Participation.Shells {
				if !validTaskUID(view.Instance) || view.Owner != "" && view.Owner != identityFromUID(canonicalTaskUID(view.Owner)) {
					return fmt.Errorf("invalid shell or parent ID in participation refresh")
				}
			}
			observed := make(map[string]shellView, len(r.Participation.Shells))
			for _, view := range r.Participation.Shells {
				observed[view.Instance] = view
			}
			var changed []participatingShell
			var removed [][]byte
			prefix := []byte(r.Host + "\x00")
			cursor := bucket.Cursor()
			for key, value := cursor.Seek(prefix); bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
				var row participatingShell
				if err := json.Unmarshal(value, &row); err != nil {
					return err
				}
				if !row.ChangedAt.Before(r.Participation.Since) {
					continue
				}
				view, found := observed[row.Instance]
				if !found {
					removed = append(removed, bytes.Clone(key))
					continue
				}
				if validSession(view.Name) == nil && view.PID > 0 && (view.State == taskActive || view.State == taskWaiting || view.State == taskDetached) {
					row.Owner = view.Owner
					row.Name, row.State, row.PID = view.Name, view.State, view.PID
					row.Shell, row.Program = view.Shell, view.Program
					row.Pinned, row.PinUntil, row.ChangedAt = view.Pinned, view.PinUntil, now
					changed = append(changed, row)
				}
			}
			for _, key := range removed {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
			for _, row := range changed {
				if err := writeParticipation(bucket, participationKey(row.Host, row.Instance), row); err != nil {
					return err
				}
			}
			return nil
		}
		return fmt.Errorf("invalid metadata operation")
	})
}
