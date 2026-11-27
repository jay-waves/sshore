package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
	bolt "go.etcd.io/bbolt"
)

const historyLock = "history.lock"

// historyTransaction opens the account database only for the supplied transaction.
// The separate lock coordinates opening the database with removal by reset.
func historyTransaction(root string, write bool, run func(*bolt.Tx) error) error {
	return stateDBTransaction(root, filepath.Join("connections", "participation.db"), write, run)
}

// A read of a missing database succeeds without invoking run.
func stateDBTransaction(root, file string, write bool, run func(*bolt.Tx) error) error {
	path := filepath.Join(root, file)
	subdir := filepath.Dir(file)
	if !write {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	dir, err := prepareStateDir(root, "")
	if err != nil {
		return err
	}
	lock, err := lockStateFile(filepath.Join(dir, historyLock))
	if err != nil {
		return err
	}
	defer lock.Close()
	if write {
		if err := checkDeploymentAdmission(); err != nil {
			return err
		}
		if _, err := prepareStateDir(root, subdir); err != nil {
			return err
		}
	} else {
		if err := sys.PrivateStatePath(filepath.Dir(path), true); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{
		ReadOnly: !write, Timeout: time.Second, OpenFile: openHistoryFile,
	})
	if !write && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open state database: %w", err)
	}
	defer db.Close()
	if write {
		return db.Update(run)
	}
	return db.View(run)
}

func openHistoryFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	err := sys.PrivateStatePath(path, false)
	if err == nil {
		return os.OpenFile(path, flags&^os.O_CREATE, mode)
	}
	if flags&os.O_CREATE == 0 || !errors.Is(err, os.ErrNotExist) {
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

func updateHistory(run func(*bolt.Tx) error) error {
	root, err := statePath("")
	if err != nil {
		return err
	}
	return historyTransaction(root, true, run)
}

// A read of a missing key leaves dst untouched.
func readBucketJSON(bucket *bolt.Bucket, key []byte, dst any) (bool, error) {
	if bucket == nil {
		return false, nil
	}
	data := bucket.Get(key)
	if data == nil {
		return false, nil
	}
	return true, json.Unmarshal(data, dst)
}

func readHistoryRows[T any](tx *bolt.Tx, name []byte, validate func(T) error) ([]T, error) {
	bucket := tx.Bucket(name)
	if bucket == nil {
		return nil, nil
	}
	var rows []T
	cursor := bucket.Cursor()
	for key, data := cursor.Last(); key != nil; key, data = cursor.Prev() {
		var row T
		if err := json.Unmarshal(data, &row); err != nil {
			return nil, err
		}
		if err := validate(row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func accountHistoryRows[T any](name []byte, validate func(T) error) (rows []T, err error) {
	root, err := statePath("")
	if err != nil {
		return nil, err
	}
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		rows, err = readHistoryRows(tx, name, validate)
		return err
	})
	return rows, err
}
