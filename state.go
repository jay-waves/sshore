package main

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
	bolt "go.etcd.io/bbolt"
)

// writeStateJSON encodes metadata before replacing its private state file.
func writeStateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeStateFile(path, data)
}

func marshalBoundedStateJSON(path string, value any, maxBytes int) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return nil, fmt.Errorf("state record is too large: %s", path)
	}
	return data, nil
}

// writeStateFile replaces metadata only after a complete private write. File transfers use
// no-overwrite publication instead; pending approvals use exclusive creation.
func writeStateFile(path string, data []byte) error {
	staged, err := sys.StageSiblingFile(path, ".state-*", func(f *os.File) error {
		if err := sys.PrivateStatePath(f.Name(), false); err != nil {
			return err
		}
		return writeFull(f, data)
	})
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return os.Rename(staged, path)
}

// Inside the daemon and on deployed hosts the state directory is already known.
func fixedStateRoot() string {
	if hub := accountActivity.Load(); hub != nil && hub.root != "" {
		return hub.root
	}
	return deployedStateRoot()
}

func statePath(name string) (string, error) {
	if root := fixedStateRoot(); root != "" {
		return filepath.Join(root, name), nil
	}
	root, _, err := accountDatabaseLocation(time.Second)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// Resolve the directory and the live bucket generation from one ownership probe.
func accountDatabaseLocation(wait time.Duration) (root, instance string, err error) {
	record, err := daemonRecordWithin(wait)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	if root := fixedStateRoot(); root != "" {
		return root, record.UID, nil
	}
	root, err = configuredStateRoot(record)
	return root, record.UID, err
}

func configuredStateRoot(record localShellRecord) (string, error) {
	root := os.Getenv("SSHORE_STATE_DIR")
	explicit := root != ""
	if !explicit {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(root, "sshore")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if record.Name == "" {
		return root, nil
	}
	if !filepath.IsAbs(record.Directory) {
		return "", fmt.Errorf("invalid account daemon state root")
	}
	if !explicit {
		return record.Directory, nil
	}
	same := filepath.Clean(root) == filepath.Clean(record.Directory)
	if !same {
		a, aErr := os.Stat(root)
		b, bErr := os.Stat(record.Directory)
		same = aErr == nil && bErr == nil && os.SameFile(a, b)
	}
	if !same {
		return "", fmt.Errorf("account daemon uses %s; SSHORE_STATE_DIR cannot select another root while it is running", record.Directory)
	}
	return record.Directory, nil
}

// Dir resolves and prepares a private subdirectory under the configured state root.
func stateDir(name string) (string, error) {
	root, err := statePath("")
	if err != nil {
		return "", err
	}
	return prepareStateDir(root, name)
}

func prepareStateDir(root, name string) (string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := sys.PrivateStatePath(root, true); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if dir == root {
		return root, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := sys.PrivateStatePath(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}

// Identity is advisory account metadata, never an authentication credential.
func lockIdentity() (*sys.FileLock, string, error) {
	dir, err := stateDir("identity")
	if err != nil {
		return nil, "", err
	}
	f, err := lockStateFile(filepath.Join(dir, "state.lock"))
	return f, dir, err
}

func openPrivateFileLock(path string) (*sys.FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = sys.PrivateStatePath(path, false); err != nil {
		f.Close()
		return nil, err
	}
	return sys.NewFileLock(f), nil
}

func tryPrivateFileLock(path string) (*sys.FileLock, error) {
	lock, err := openPrivateFileLock(path)
	if err != nil {
		return nil, err
	}
	if err := lock.TryLock(); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// lockStateFile serializes private metadata updates. Closing the lock releases it.
func lockStateFile(path string) (*sys.FileLock, error) {
	return acquireStateFileLock(path, false)
}

func acquireStateFileLock(path string, shared bool) (*sys.FileLock, error) {
	lock, err := openPrivateFileLock(path)
	if err != nil {
		return nil, err
	}
	try := lock.TryLock
	if shared {
		try = lock.TryReadLock
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = try()
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, sys.ErrFileLockBusy) || time.Now().After(deadline) {
			lock.Close()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func persistentMachineIdentity(reset bool) (string, error) {
	lock, dir, err := lockIdentity()
	if err != nil {
		return "", err
	}
	defer lock.Close()
	path := filepath.Join(dir, "salt")
	salt, err := os.ReadFile(path)
	if reset || os.IsNotExist(err) {
		salt = make([]byte, 32)
		rand.Read(salt)
		if err = writeStateFile(path, salt); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if len(salt) != 32 {
		return "", fmt.Errorf("invalid identity salt; run sshore reset")
	}
	sum := md5.Sum(append(append([]byte("sshore\x00"+sys.MachineID()+"\x00"), salt...), '\x00'))
	return hex.EncodeToString(sum[:]), nil
}

// Remember entry points locally; different routes are not proof of a clone.
func observeIdentityRoute(id, host string, ssh []string) (bool, error) {
	if !validMachineID(id) || host == "" {
		return false, nil
	}
	routeData, err := json.Marshal(struct {
		Host string
		SSH  []string
	}{host, ssh})
	if err != nil {
		return false, err
	}
	route := sha256.Sum256(routeData)
	var warn bool
	err = updateHistory(func(tx *bolt.Tx) error {
		machines, err := tx.CreateBucketIfNotExists([]byte("identity-routes"))
		if err != nil {
			return err
		}
		routes, err := machines.CreateBucketIfNotExists([]byte(id))
		if err != nil {
			return err
		}
		if routes.Get(route[:]) != nil {
			return nil
		}
		first, _ := routes.Cursor().First()
		warn = first != nil
		return routes.Put(route[:], []byte{1})
	})
	return warn, err
}

// This identifier is advisory display metadata, never connection or account authority.
func localMachineIdentity() (string, string) {
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	id, _ := persistentMachineIdentity(false)
	return id, host
}

func validMachineID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
