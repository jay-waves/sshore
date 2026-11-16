package tmux

import (
	"strings"
	"testing"
)

func TestMouseStateAcrossReadBoundaries(t *testing.T) {
	input := "\x1b[?1003;1006h\x1b[?1002h\x1b[?1006l\x1b[?1016h"
	for split := 0; split <= len(input); split++ {
		var s InputState
		s.Observe([]byte(input[:split]))
		s.Observe([]byte(input[split:]))
		got := string(s.Replay())
		want := MouseResetSequence() + "\x1b[?1002h\x1b[?1016h"
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
		var s InputState
		s.Observe([]byte(input))
		if len(s.Replay()) != 0 {
			t.Errorf("interpreted %q: %q", input, s.Replay())
		}
	}
}

func TestMouseStateDisabledWhileDetached(t *testing.T) {
	var s InputState
	s.Observe([]byte("\x1b[?1003;1006h"))
	s.Observe([]byte("\x1b[?1003;1006l"))
	if string(s.Replay()) != MouseResetSequence() {
		t.Fatalf("stale mouse state: %q", s.Replay())
	}
	s.Observe([]byte("\x1b[?1003;1006h\x1bc"))
	if string(s.Replay()) != MouseResetSequence()+"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b[?2031l\x1b[?2033l\x1b[?2048l\x1b>" {
		t.Fatalf("RIS failed: %q", s.Replay())
	}
}

func TestMouseReplayPreservesPartialSequence(t *testing.T) {
	var s InputState
	s.Observe([]byte("\x1b[?100"))
	var resumed InputState
	resumed.Observe(s.Replay())
	resumed.Observe([]byte("3;1006h"))
	if string(resumed.Replay()) != MouseResetSequence()+"\x1b[?1003h\x1b[?1006h" {
		t.Fatalf("lost partial CSI: %q", resumed.Replay())
	}
}

func TestMouseSavedModes(t *testing.T) {
	var s InputState
	s.Observe([]byte("\x1b[?1003;1006h\x1b[?1003;1006s\x1b[?1003;1006l"))
	var resumed InputState
	resumed.Observe(s.Replay())
	if resumed.enabled != s.enabled || resumed.saved != s.saved {
		t.Fatal("saved/current state not preserved")
	}
	resumed.Observe([]byte("\x1b[?1003;1006r"))
	if !resumed.enabled[4] || !resumed.enabled[6] {
		t.Fatalf("saved modes lost: %+v", resumed)
	}
}

func TestFocusPasteStateAcrossAttachment(t *testing.T) {
	input := "\x1b[?1004;2004h\x1b[?1004;2004s\x1b[?1004l"
	for split := 0; split <= len(input); split++ {
		var state InputState
		state.Observe([]byte(input[:split]))
		state.Observe([]byte(input[split:]))
		var resumed InputState
		resumed.Observe(state.Replay())
		// Restore happens after reattachment without another save.
		resumed.Observe([]byte("\x1b[?1004;2004r"))
		got := string(resumed.Replay())
		if !strings.HasSuffix(got, "\x1b[?1004h\x1b[?2004h") {
			t.Fatalf("split %d: lost input modes %q", split, got)
		}
		state.Observe([]byte("\x1b[?1004;2004l"))
		got = string(state.Replay())
		// Saved enabled values can appear in the replay, but final current values are off.
		var disabled InputState
		disabled.Observe([]byte(got))
		if disabled.enabled[9] || disabled.enabled[10] {
			t.Fatalf("split %d: stale enabled mode", split)
		}
	}
}

func BenchmarkTerminalInputObserve(b *testing.B) {
	for _, tc := range []struct{ name, data string }{
		{"plain", strings.Repeat("build output: ordinary UTF-8 中文 text\r\n", 180)},
		{"styled", strings.Repeat("\x1b[32mbuild output\x1b[0m ordinary text\r\n", 180)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := []byte(tc.data)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			var state InputState
			for b.Loop() {
				state.Observe(data)
			}
		})
	}
}

func TestInputModesBulkMatchesBytewise(t *testing.T) {
	data := []byte(strings.Repeat("普通 output\r\n", 900) + "\x1b[?1004;2004h\x1b]opaque\x1b[?2004l\x07" + strings.Repeat("more output", 900) + "\x1b[?1003;1006h\x1b[?1004s\x1b[?1004l\x1b[?2004")
	var bulk, bytewise InputState
	bulk.Observe(data)
	for i := range data {
		bytewise.Observe(data[i : i+1])
	}
	if string(bulk.Replay()) != string(bytewise.Replay()) {
		t.Fatalf("bulk=%q bytewise=%q", bulk.Replay(), bytewise.Replay())
	}
	bulk.Observe([]byte("l"))
	bytewise.Observe([]byte("l"))
	if string(bulk.Replay()) != string(bytewise.Replay()) {
		t.Fatal("partial CSI diverged")
	}
}

func TestAttachInsideOpaqueControlString(t *testing.T) {
	for _, kind := range []byte{']', 'P', '^', '_', 'X'} {
		end := "\x1b\\"
		if kind == ']' {
			end = "\a"
		}
		input := "\x1b" + string(kind) + "payload\x1b[?1003h" + end + "\x1b[?2004h"
		for split := 2; split < len(input); split++ {
			var original, resumed InputState
			original.Observe([]byte(input[:split]))
			resumed.Observe(original.Replay())
			original.Observe([]byte(input[split:]))
			resumed.Observe([]byte(input[split:]))
			if resumed.enabled != original.enabled || resumed.MouseEnabled() {
				t.Fatalf("kind %c split %d: opaque payload became input modes", kind, split)
			}
		}
	}
}

func TestApplicationKeysAcrossAttachment(t *testing.T) {
	for _, input := range []string{
		"\x1b[?1h\x1b=",
		"\x1b[?1h\x1b=\x1b[?1l\x1b>",
		"\x1b[?1h\x1b[?1s\x1b[?1l\x1b=",
		"\x1b[?1h\x1b=\x1bc",
		"\x1b]ignored\x1b=\a\x1bP\x1b[?1h\x1b\\",
	} {
		for split := 0; split <= len(input); split++ {
			var state, resumed InputState
			state.Observe([]byte(input[:split]))
			resumed.Observe(state.Replay())
			state.Observe([]byte(input[split:]))
			resumed.Observe([]byte(input[split:]))
			if state.enabled != resumed.enabled || state.keypad != resumed.keypad || state.saved != resumed.saved {
				t.Fatalf("input %q split %d: application key state changed", input, split)
			}
		}
	}
	var state InputState
	state.Observe([]byte("\x1b[?1h\x1b[?1s\x1b[?1l\x1b="))
	var resumed InputState
	resumed.Observe(state.Replay())
	resumed.Observe([]byte("\x1b[?1r"))
	if !resumed.enabled[11] || !resumed.keypad {
		t.Fatal("saved application cursor mode or keypad lost on attachment")
	}
	resumed.Observe([]byte(terminalInputResetSequence()))
	if resumed.enabled[11] || resumed.keypad {
		t.Fatal("application keys left active at local prompt")
	}
}

func TestInputStateRISResetsFreshAttachment(t *testing.T) {
	var application InputState
	application.Observe([]byte("\x1bc"))
	var terminal InputState
	terminal.Observe([]byte("\x1b[?1;1003;1004;2004h\x1b="))
	terminal.Observe(application.Replay())
	for i, on := range terminal.enabled {
		if on {
			t.Errorf("RIS replay left mode %s enabled", inputModes[i])
		}
	}
	if terminal.keypad {
		t.Error("RIS replay left application keypad enabled")
	}
}

func TestExtendedReportsAcrossAttachment(t *testing.T) {
	for _, mode := range []string{"2031", "2033", "2048"} {
		t.Run(mode, func(t *testing.T) {
			input := "\x1b[?" + mode + "h"
			for cut := 0; cut <= len(input); cut++ {
				var state, resumed InputState
				state.Observe([]byte(input[:cut]))
				resumed.Observe(state.Replay())
				resumed.Observe([]byte(input[cut:]))
				if !strings.Contains(string(resumed.Replay()), input) {
					t.Fatalf("cut %d lost reporting", cut)
				}
				resumed.Observe([]byte(terminalInputResetSequence()))
				if strings.Contains(string(resumed.Replay()), input) {
					t.Fatalf("cut %d left reporting enabled", cut)
				}
			}
		})
	}
}
