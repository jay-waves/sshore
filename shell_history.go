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

// Recent histories contain at most 32 rows, ordered by bbolt's bucket sequence.
func deleteRecent[T any](bucket *bolt.Bucket, match func(T) bool) error {
	if bucket == nil {
		return nil
	}
	cursor := bucket.Cursor()
	for key, data := cursor.First(); key != nil; key, data = cursor.Next() {
		var row T
		if err := json.Unmarshal(data, &row); err != nil {
			return err
		}
		if match(row) {
			return cursor.Delete()
		}
	}
	return nil
}

func promoteRecent[T any](tx *bolt.Tx, name []byte, row T, same func(T) bool) error {
	bucket, err := tx.CreateBucketIfNotExists(name)
	if err != nil {
		return err
	}
	if err := deleteRecent(bucket, same); err != nil {
		return err
	}
	// Count with a fresh cursor because the transaction may already have deleted a row.
	cursor := bucket.Cursor()
	n := 0
	for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
		n++
	}
	if n >= recentHistoryLimit {
		cursor.First()
		if err := cursor.Delete(); err != nil {
			return err
		}
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
