package main

import (
	"encoding/binary"
	json "encoding/json/v2"

	bolt "go.etcd.io/bbolt"
)

var (
	connectionBucket = []byte("recent-connections")
	attachmentBucket = []byte("recent-detaches")
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

func readShellCandidates(root string) (observed []participatingShell, records []recentConnection, detached []recentAttachment, err error) {
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		observed, err = participatingShells(tx)
		if err != nil {
			return err
		}
		records, err = readHistoryRows(tx, connectionBucket, validateShellHistoryConnection)
		if err != nil {
			return err
		}
		detached, err = readHistoryRows(tx, attachmentBucket, validateRecentAttachment)
		return err
	})
	return
}

func rememberShellExit(o options) error {
	return updateHistory(func(tx *bolt.Tx) error {
		if err := removeRecentDetach(tx, o); err != nil {
			return err
		}
		if o.local || !validTaskUID(o.instance) {
			return nil
		}
		return forgetParticipation(tx, o.host, o.instance)
	})
}
