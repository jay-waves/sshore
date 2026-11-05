package main

import (
	"strings"
	"testing"
)

func TestMouseStateAcrossReadBoundaries(t *testing.T) {
	input := "\x1b[?1003;1006h\x1b[?1002h\x1b[?1006l\x1b[?1016h"
	for split := 0; split <= len(input); split++ {
		var s terminalMouseState
		s.observe([]byte(input[:split]))
		s.observe([]byte(input[split:]))
		got := string(s.replay())
		want := mouseResetSequence() + "\x1b[?1002h\x1b[?1016h"
		if got != want {
			t.Fatalf("split %d: %q", split, got)
		}
	}
}

func TestMouseStateIgnoresControlStringsAndMalformedCSI(t *testing.T) {
	for _, input := range []string{
		"\x1b]title\x1b[?1003h\x07", "\x1bP\x1b[?1003h\x1b\\", "\x1b_\x1b[?1003h\x1b\\",
		"\x1b[?1003:1h", "\x1b[?1003;bad h", "\x1b[?" + strings.Repeat("0", 129) + "1003h",
		"\x1b[?1003\x18h",
	} {
		var s terminalMouseState
		s.observe([]byte(input))
		if len(s.replay()) != 0 {
			t.Errorf("interpreted %q: %q", input, s.replay())
		}
	}
}

func TestMouseStateDisabledWhileDetached(t *testing.T) {
	var s terminalMouseState
	s.observe([]byte("\x1b[?1003;1006h"))
	s.observe([]byte("\x1b[?1003;1006l"))
	if string(s.replay()) != mouseResetSequence() {
		t.Fatalf("stale mouse state: %q", s.replay())
	}
	s.observe([]byte("\x1b[?1003;1006h\x1bc"))
	if string(s.replay()) != mouseResetSequence() {
		t.Fatalf("RIS failed: %q", s.replay())
	}
}

func TestMouseReplayPreservesPartialSequence(t *testing.T) {
	var s terminalMouseState
	s.observe([]byte("\x1b[?100"))
	var resumed terminalMouseState
	resumed.observe(s.replay())
	resumed.observe([]byte("3;1006h"))
	if string(resumed.replay()) != mouseResetSequence()+"\x1b[?1003h\x1b[?1006h" {
		t.Fatalf("lost partial CSI: %q", resumed.replay())
	}
}

func TestMouseSavedModes(t *testing.T) {
	var s terminalMouseState
	s.observe([]byte("\x1b[?1003;1006h\x1b[?1003;1006s\x1b[?1003;1006l"))
	var resumed terminalMouseState
	resumed.observe(s.replay())
	if resumed.enabled != s.enabled || resumed.saved != s.saved {
		t.Fatal("saved/current state not preserved")
	}
	resumed.observe([]byte("\x1b[?1003;1006r"))
	if !resumed.enabled[4] || !resumed.enabled[6] {
		t.Fatalf("saved modes lost: %+v", resumed)
	}
}
