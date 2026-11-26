package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	return identityFromUID(r.UID)
}

func identityFromUID(uid string) string {
	if !validTaskUID(uid) {
		return ""
	}
	return "task:" + uid
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

// Displayed IDs stay six characters; selectors may use shorter unique prefixes.
func isShortTaskID(name string) bool { return len(name) >= 6 && isTaskIDPrefix(name) }

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
	if isTaskIDPrefix(name) {
		return usage("2–32 hexadecimal characters are reserved for task IDs; choose another name")
	}
	return nil
}
