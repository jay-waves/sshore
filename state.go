package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeStateJSON encodes metadata before replacing its private state file.
func writeStateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeStateFile(path, data)
}

// writeStateFile replaces metadata only after a complete private write. File transfers use
// no-overwrite publication instead; pending approvals use exclusive creation.
func writeStateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = privateStatePath(f.Name(), false); err != nil {
		return err
	}
	if err = sshio.WriteFull(f, data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Path resolves a state path without creating directories or changing permissions.
func statePath(name string) (string, error) {
	if hub := accountActivity.Load(); hub != nil && hub.root != "" {
		return filepath.Join(hub.root, name), nil
	}
	root := os.Getenv("SSHORE_STATE_DIR")
	if root == "" {
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
	root, err = daemonStateRoot(root, os.Getenv("SSHORE_STATE_DIR") != "")
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
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err = privateStatePath(root, true); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err = privateStatePath(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}

// Identity is advisory account metadata, never an authentication credential.
func lockIdentity() (*os.File, string, error) {
	dir, err := stateDir("identity")
	if err != nil {
		return nil, "", err
	}
	f, err := lockStateFile(filepath.Join(dir, "state.lock"))
	return f, dir, err
}

// lockStateFile serializes private metadata updates. Closing the file releases it.
func lockStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateStatePath(path, false); err != nil {
		f.Close()
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = lockResourceState(f)
		if err == nil {
			return f, nil
		}
		if !resourceLockBusy(err) || time.Now().After(deadline) {
			f.Close()
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
		if _, err = rand.Read(salt); err != nil {
			return "", err
		}
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
