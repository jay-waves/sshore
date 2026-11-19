package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func canonicalTaskUID(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if ok && (kind == "task" || kind == "peer-task") && validTaskUID(uid) {
		return uid
	}
	return ""
}

func taskUID(key string) string {
	if key == "" {
		return ""
	}
	if uid := canonicalTaskUID(key); uid != "" {
		return uid
	}
	// Legacy observations retain a deterministic reference without allocating state.
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func canonicalTaskCode(key string) string {
	if uid := canonicalTaskUID(key); uid != "" {
		return uid[:6]
	}
	return ""
}

func validTaskUID(uid string) bool {
	return len(uid) == 32 && strings.Trim(uid, "0123456789abcdef") == ""
}

func newTaskUID() string {
	var random [16]byte
	rand.Read(random[:])
	return hex.EncodeToString(random[:])
}

func taskShortCode(key string) string {
	uid := taskUID(key)
	return uid[:min(6, len(uid))]
}

func resourceIdentity(r resourceRecord) string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	if r.Token == "" {
		return ""
	}
	return fmt.Sprintf("%s:%x", r.Kind, sha256.Sum256([]byte(r.Token)))
}

func remoteShellIdentity(host, instance string) string {
	if validTaskUID(instance) {
		return "task:" + instance
	}
	if instance == "" {
		return ""
	}
	return "remote:" + host + ":" + instance
}

func validSession(name string) error {
	if len(name) < 1 || len(name) > 48 {
		return usage("name must contain 1–48 ASCII characters")
	}
	for i, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return usage("invalid name: use letters, digits, _ and -")
		}
	}
	return nil
}

var errObjectNameInUse = errors.New("name is already in use")

type taskIdentitySelection struct{ name, uid string }

// Selection is advisory. Only insertion into the daemon task table claims a
// name; there is no reservation to recover or transfer between processes.
func chooseTaskIdentity(kind, requested string) (*taskIdentitySelection, error) {
	return chooseTaskIdentityExcept(kind, requested, "")
}
func chooseTaskIdentityExcept(kind, requested, exceptID string) (*taskIdentitySelection, error) {
	if requested != "" {
		if err := validSession(requested); err != nil {
			return nil, err
		}
	}
	uid := exceptID
	if uid == "" {
		uid = newTaskUID()
	} else if !validTaskUID(uid) {
		return nil, fmt.Errorf("invalid task ID")
	}

	name, err := availableTaskName(uid, requested, exceptID != "")
	if err != nil {
		return nil, err
	}
	if name == "" {
		if requested == "" {
			return nil, fmt.Errorf("task UID already exists")
		}
		return nil, fmt.Errorf("%s/%s: %w", kind, requested, errObjectNameInUse)
	}
	return &taskIdentitySelection{name: name, uid: uid}, nil
}

func availableTaskName(uid, requested string, reuse bool) (string, error) {
	if active := accountActivity.Load(); active != nil && active.tasks != nil {
		return active.tasks.availableName(uid, requested, reuse), nil
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		if requested == "" {
			requested = uid[:6]
		}
		return requested, nil
	}
	if err != nil {
		return "", err
	}
	query := url.Values{"uid": {uid}, "name": {requested}}
	if reuse {
		query.Set("reuse", "1")
	}
	var name string
	err = daemonJSON(context.Background(), record, "/tasks/name?"+query.Encode(), nil, &name)
	return name, err
}

// Displayed IDs stay six characters; selectors may use shorter unique prefixes.
func isShortTaskID(name string) bool { return len(name) >= 6 && isTaskIDPrefix(name) }

func shortTaskPrefix(name string) string {
	if isShortTaskID(name) {
		return strings.ToLower(name)
	}
	return ""
}

func isTaskIDPrefix(name string) bool {
	if len(name) < 2 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func validAlias(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if isShortTaskID(name) {
		return usage("6–32 hexadecimal characters are reserved for task IDs; choose another alias")
	}
	return nil
}
