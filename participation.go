package main

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"time"

	"github.com/jay-waves/sshore/internal/taskid"
	bolt "go.etcd.io/bbolt"
)

type directSSHRoute struct {
	Directory string   `json:",omitempty"`
	SSH       []string `json:",omitempty"`
}

// Participation records only tasks opened through this client account.
type participatingShell struct {
	Route                *directSSHRoute `json:",omitempty"`
	HostInfo             hostInfo        `json:",omitzero"`
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

func validateParticipatingShell(row participatingShell) error {
	switch {
	case validSSHDestination(row.Host) != nil:
		return fmt.Errorf("remote shell has an invalid SSH destination")
	case !taskid.Valid(row.Instance):
		return fmt.Errorf("remote shell has an invalid task ID")
	case row.Owner != "" && row.Owner != taskid.Key(taskid.ParseKey(row.Owner)):
		return fmt.Errorf("remote shell has an invalid parent task ID")
	case validSession(row.Name) != nil:
		return fmt.Errorf("remote shell has an invalid session name")
	case row.PID <= 0:
		return fmt.Errorf("remote shell has invalid PID %d (expected a positive PID)", row.PID)
	case !taskid.Valid(row.Attachment):
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

func participatingShells(tx *bolt.Tx) ([]participatingShell, error) {
	return readHistoryRows(tx, participationBucket, validateParticipatingShell)
}

func writeParticipation(bucket *bolt.Bucket, key []byte, row participatingShell) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

func observeParticipation(tx *bolt.Tx, row participatingShell) error {
	if err := validateParticipatingShell(row); err != nil {
		return err
	}
	bucket, err := tx.CreateBucketIfNotExists(participationBucket)
	if err != nil {
		return err
	}
	key := participationKey(row.Host, row.Instance)
	var previous participatingShell
	found, err := readBucketJSON(bucket, key, &previous)
	if err != nil {
		return err
	}
	if !found && bucket.Stats().KeyN >= participationLimit {
		return fmt.Errorf("participating shell limit reached")
	}
	if row.HostInfo == (hostInfo{}) {
		row.HostInfo = previous.HostInfo
	}
	row.ChangedAt = time.Now()
	return writeParticipation(bucket, key, row)
}

// Only the current attachment may change the observation and its detach history.
func setParticipationState(tx *bolt.Tx, row participatingShell) error {
	if err := validateParticipatingShell(row); err != nil {
		return err
	}
	key := participationKey(row.Host, row.Instance)
	var previous participatingShell
	bucket := tx.Bucket(participationBucket)
	found, err := readBucketJSON(bucket, key, &previous)
	if err != nil || !found || previous.Attachment != row.Attachment {
		return err
	}
	if row.State == taskDetached {
		previous.Name = row.Name
	}
	previous.State, previous.ChangedAt = row.State, time.Now()
	previous.Pinned, previous.PinUntil = row.Pinned, row.PinUntil
	if err := writeParticipation(bucket, key, previous); err != nil {
		return err
	}
	if previous.State == taskDetached && previous.Route != nil {
		row := recentAttachment{Host: previous.Host, Instance: previous.Instance}
		return promoteRecent(tx, attachmentBucket, row, func(old recentAttachment) bool {
			return old.Instance == row.Instance
		})
	}
	return nil
}

// Late terminal output cannot revive a detached or replaced attachment.
func touchParticipation(attachment participatingShell, seen time.Time) error {
	return updateHistory(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(participationBucket)
		key := participationKey(attachment.Host, attachment.Instance)
		var row participatingShell
		found, err := readBucketJSON(bucket, key, &row)
		if err != nil || !found || row.Attachment != attachment.Attachment || row.State != taskActive || !row.ChangedAt.Before(seen) {
			return err
		}
		row.ChangedAt = seen
		return writeParticipation(bucket, key, row)
	})
}

func forgetParticipation(tx *bolt.Tx, host, instance string) error {
	if !taskid.Valid(instance) || validSSHDestination(host) != nil {
		return fmt.Errorf("invalid participating shell identity")
	}
	if bucket := tx.Bucket(participationBucket); bucket != nil {
		return bucket.Delete(participationKey(host, instance))
	}
	return nil
}

func reconcileParticipatingShells(host string, peer hostInfo, shells []shellView, since time.Time) error {
	if validSSHDestination(host) != nil {
		return fmt.Errorf("invalid participating shell refresh")
	}
	if peer.User == "" || peer.Address == "" || peer.Port == "" {
		return nil
	}
	observed := make(map[string]shellView, len(shells))
	for _, view := range shells {
		if !taskid.Valid(view.Instance) || view.Owner != "" && view.Owner != taskid.Key(taskid.ParseKey(view.Owner)) {
			return fmt.Errorf("invalid shell or parent ID in participation refresh")
		}
		observed[view.Instance] = view
	}
	return updateHistory(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(participationBucket)
		if bucket == nil {
			return nil
		}
		now := time.Now()
		var changed []participatingShell
		var removed [][]byte
		prefix := []byte(host + "\x00")
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(prefix); bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
			var row participatingShell
			if err := json.Unmarshal(value, &row); err != nil {
				return err
			}
			if !row.ChangedAt.Before(since) ||
				row.HostInfo.User != peer.User || row.HostInfo.Address != peer.Address || row.HostInfo.Port != peer.Port {
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
	})
}
