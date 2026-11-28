package main

import (
	"strings"
	"testing"
)

func TestStoredKeysAndUserPrefixes(t *testing.T) {
	uid := newTaskUID()
	for _, key := range []string{taskKey(uid), "peer-task:" + uid} {
		if canonicalTaskUID(key) != uid || canonicalTaskCode(key) != uid[:6] || taskObservationUID(key) != uid {
			t.Fatalf("identity lost for stored key %q", key)
		}
	}
	if validTaskUID(strings.Repeat("A", 32)) || !isTaskIDPrefix("AF") || isShortTaskID("AF") || !isShortTaskID("ABCDEF") {
		t.Fatal("stored UIDs require lowercase, selectors accept uppercase from two characters")
	}
	const old = "local:previous"
	if canonicalTaskUID(old) != "" || canonicalTaskCode(old) != "" || !validTaskUID(taskObservationUID(old)) || shortTaskObservation(old) != taskObservationUID(old)[:6] {
		t.Fatal("legacy observation must have a display ID without becoming a canonical task key")
	}
}
