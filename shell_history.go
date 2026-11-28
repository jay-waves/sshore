package main

import (
	"encoding/binary"
	json "encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jay-waves/sshore/internal/taskid"
	bolt "go.etcd.io/bbolt"
)

var (
	connectionBucket = []byte("recent-connections")
	attachmentBucket = []byte("remote-detach-order")
)

const recentHistoryLimit = 32

// Scan newest first. On promotion, reserve one slot by evicting the oldest
// row if the bounded history contains no match.
func pruneRecent[T any](bucket *bolt.Bucket, keep int, match func(T) bool) error {
	if bucket == nil {
		return nil
	}
	cursor := bucket.Cursor()
	for key, data := cursor.Last(); key != nil; key, data = cursor.Prev() {
		var row T
		if err := json.Unmarshal(data, &row); err != nil {
			return err
		}
		if match(row) || keep == 0 {
			return cursor.Delete()
		}
		keep--
	}
	return nil
}

func promoteRecent[T any](tx *bolt.Tx, name []byte, row T, same func(T) bool) error {
	bucket, err := tx.CreateBucketIfNotExists(name)
	if err != nil {
		return err
	}
	if err := pruneRecent(bucket, recentHistoryLimit-1, same); err != nil {
		return err
	}
	seq, err := bucket.NextSequence()
	if err != nil {
		return err
	}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return bucket.Put(binary.BigEndian.AppendUint64(nil, seq), data)
}

func removeRecentDetach(tx *bolt.Tx, instance string) error {
	return pruneRecent(tx.Bucket(attachmentBucket), recentHistoryLimit, func(row recentAttachment) bool {
		return row.Instance == instance
	})
}

func rememberShellExit(o options) error {
	if o.local || !taskid.Valid(o.instance) {
		return nil
	}
	return updateHistory(func(tx *bolt.Tx) error {
		if err := removeRecentDetach(tx, o.instance); err != nil {
			return err
		}
		return forgetParticipation(tx, o.host, o.instance)
	})
}

type recentConnection struct {
	Hosted                   bool              `json:",omitempty"`
	RouteTarget              bool              `json:",omitempty"`
	Parent                   *recentConnection `json:",omitempty"`
	RouteToken               string            `json:",omitempty"`
	Instance                 string            `json:",omitempty"`
	Host, Session, Directory string
	SSH                      []string
}

func (r recentConnection) viaShell() bool { return r.Hosted || r.Parent != nil }

const maxConnectionHops = 64

func validateRecentConnection(r recentConnection) error {
	for depth, hop := 0, &r; hop != nil; depth, hop = depth+1, hop.Parent {
		if depth >= maxConnectionHops {
			return fmt.Errorf("too many connection hops")
		}
		if hop.Hosted && (hop.Host != "local" || !taskid.Valid(hop.Instance) || len(hop.SSH) != 0 || hop.Parent != nil) {
			return fmt.Errorf("invalid hosted shell connection")
		}
		if hop.Parent != nil {
			token := hop.RouteToken
			if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" || hop.Instance == "" || hop.Parent.Instance == "" {
				return fmt.Errorf("invalid saved shell route")
			}
		} else if hop.RouteToken != "" || hop.RouteTarget {
			return fmt.Errorf("shell route parent missing")
		}
		if validSSHDestination(hop.Host) != nil || validSession(hop.Session) != nil {
			return fmt.Errorf("invalid recent connection")
		}
		if hop.Directory != "" && !filepath.IsAbs(hop.Directory) {
			return fmt.Errorf("invalid recent connection directory")
		}
		if len(hop.SSH) > 0 {
			if _, _, err := parseSSHOptions(hop.SSH); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateShellHistoryConnection(record recentConnection) error {
	if record.viaShell() {
		return fmt.Errorf("shell history must use a direct SSH connection")
	}
	return validateRecentConnection(record)
}

type recentAttachment struct {
	Host, Instance string
}

func validateRecentAttachment(row recentAttachment) error {
	if !taskid.Valid(row.Instance) || validSSHDestination(row.Host) != nil {
		return fmt.Errorf("invalid detached attachment")
	}
	return nil
}

// The SSH peer supplies eligible roots. Local history only ranks those identities.
func recentRemoteChoice(host string, targets []sessionTarget) (string, error) {
	rows, err := accountHistoryRows(attachmentBucket, validateRecentAttachment)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Host != host {
			continue
		}
		for _, target := range targets {
			if target.Instance == row.Instance {
				return target.Name, nil
			}
		}
	}
	return "", nil
}
