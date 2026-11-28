package taskid

import (
	"strings"
	"testing"
)

func TestStoredKeysAndUserPrefixes(t *testing.T) {
	uid := New()
	for _, key := range []string{Key(uid), "peer-task:" + uid} {
		if ParseKey(key) != uid || ShortKey(key) != uid[:6] || ObservationUID(key) != uid {
			t.Fatalf("identity lost for stored key %q", key)
		}
	}
	if Valid(strings.Repeat("A", 32)) || !IsPrefix("AF") || IsShort("AF") || !IsShort("ABCDEF") {
		t.Fatal("stored UIDs require lowercase, selectors accept uppercase from two characters")
	}
	const old = "local:previous"
	if ParseKey(old) != "" || ShortKey(old) != "" || !Valid(ObservationUID(old)) || ShortObservation(old) != ObservationUID(old)[:6] {
		t.Fatal("legacy observation must have a display ID without becoming a canonical task key")
	}
}
