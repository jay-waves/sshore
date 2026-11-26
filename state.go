package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
)

// readStateJSON checks privacy before opening and bounds the entire record.
func readStateJSON(path string, maxBytes int, value any) error {
	if err := privateStatePath(path, false); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return readBoundedStateJSON(f, maxBytes, value)
}

func readBoundedStateJSON(f *os.File, maxBytes int, value any) error {
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("state record is too large: %s", f.Name())
	}
	return json.Unmarshal(data, value)
}

// writeStateJSON encodes metadata before replacing its private state file.
func writeStateJSON(path string, value any) error {
	return writeBoundedStateJSON(path, value, 0)
}

func writeBoundedStateJSON(path string, value any, maxBytes int) error {
	data, err := marshalBoundedStateJSON(path, value, maxBytes)
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
	staged, err := stageSiblingFile(path, ".state-*", func(f *os.File) error {
		if err := privateStatePath(f.Name(), false); err != nil {
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

// stageSiblingFile gives the caller a closed, synced file on the destination
// filesystem. It owns cleanup until the path is returned.
func stageSiblingFile(target, pattern string, prepare func(*os.File) error) (staged string, err error) {
	f, err := os.CreateTemp(filepath.Dir(target), pattern)
	if err != nil {
		return "", err
	}
	defer func() {
		if staged == "" {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err = prepare(f); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// Path resolves a state path without creating directories or changing permissions.
func statePath(name string) (string, error) {
	if hub := accountActivity.Load(); hub != nil && hub.root != "" {
		return filepath.Join(hub.root, name), nil
	}
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
	root, err = daemonStateRoot(root, explicit)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
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
	if err := privateStatePath(root, true); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if dir == root {
		return root, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := privateStatePath(dir, true); err != nil {
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
	if err = privateStatePath(path, false); err != nil {
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
	lock, err := openPrivateFileLock(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = lock.TryLock()
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
	sum := md5.Sum(append(append([]byte("sshore\x00"+systemMachineID()+"\x00"), salt...), '\x00'))
	return hex.EncodeToString(sum[:]), nil
}

// Remember entry points locally; different routes are not proof of a clone.
func observeIdentityRoute(id, host string, ssh []string) (bool, error) {
	if !validMachineID(id) || host == "" {
		return false, nil
	}
	lock, dir, err := lockIdentity()
	if err != nil {
		return false, err
	}
	defer lock.Close()
	routeData, _ := json.Marshal(struct {
		Host string
		SSH  []string
	}{host, ssh})
	sum := md5.Sum(routeData)
	route := hex.EncodeToString(sum[:])
	path := filepath.Join(dir, id+".json")
	routes := map[string]bool{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &routes); err != nil {
			return false, err
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if routes == nil {
		routes = map[string]bool{}
	}
	if routes[route] {
		return false, nil
	}
	warn := len(routes) > 0
	routes[route] = true
	return warn, writeStateJSON(path, routes)
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
