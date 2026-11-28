package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func canonicalTaskUID(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if ok && (kind == "task" || kind == "peer-task") && validTaskUID(uid) {
		return uid
	}
	return ""
}

func taskObservationUID(key string) string {
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
	return shortTaskUID(canonicalTaskUID(key))
}

func validTaskUID(uid string) bool {
	return len(uid) == 32 && strings.Trim(uid, "0123456789abcdef") == ""
}

func newTaskUID() string {
	var random [16]byte
	rand.Read(random[:])
	return hex.EncodeToString(random[:])
}

func shortTaskObservation(key string) string {
	return shortTaskUID(taskObservationUID(key))
}

// shortTaskUID returns the display prefix of a UID, or empty for a missing UID.
func shortTaskUID(uid string) string {
	return uid[:min(6, len(uid))]
}

func taskKey(uid string) string {
	if !validTaskUID(uid) {
		return ""
	}
	return "task:" + uid
}

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
