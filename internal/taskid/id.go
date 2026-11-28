// Package taskid handles task identifiers and their stored keys.
package taskid

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func ParseKey(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if ok && (kind == "task" || kind == "peer-task") && Valid(uid) {
		return uid
	}
	return ""
}

func ObservationUID(key string) string {
	if key == "" {
		return ""
	}
	if uid := ParseKey(key); uid != "" {
		return uid
	}
	// Legacy observations retain a deterministic reference without allocating state.
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func ShortKey(key string) string {
	return Short(ParseKey(key))
}

func Valid(uid string) bool {
	return len(uid) == 32 && strings.Trim(uid, "0123456789abcdef") == ""
}

func New() string {
	var random [16]byte
	rand.Read(random[:])
	return hex.EncodeToString(random[:])
}

func ShortObservation(key string) string {
	return Short(ObservationUID(key))
}

// Short returns the display prefix of a UID, or empty for a missing UID.
func Short(uid string) string {
	return uid[:min(6, len(uid))]
}

func Key(uid string) string {
	if !Valid(uid) {
		return ""
	}
	return "task:" + uid
}

// Displayed IDs stay six characters; selectors may use shorter unique prefixes.
func IsShort(name string) bool { return len(name) >= 6 && IsPrefix(name) }

func IsPrefix(name string) bool {
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
