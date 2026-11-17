package tmux

import (
	"bytes"
	"reflect"
	"testing"
)

func collectKeys(k *Keys, chunks ...[]byte) ([]byte, []KeyAction) {
	var data []byte
	var actions []KeyAction
	for _, chunk := range chunks {
		for _, e := range k.Feed(chunk) {
			data = append(data, e.Data...)
			if e.Action != KeyNone {
				actions = append(actions, e.Action)
			}
		}
	}
	return data, actions
}
func TestPrefixKeysAcrossReads(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		action      KeyAction
	}{
		{"a\x02\x02b", "a\x02b", KeyNone}, {"before\x02dignored", "before", KeyDetach}, {"a\x02rZ", "aZ", KeyRedraw}, {"\x02i", "", KeyInfo}, {"\x02?", "", KeyHelp}, {"\x02x", "\x02x", KeyNone}, {"\x02s", "\x02s", KeyNone}, {"\x02w", "\x02w", KeyNone}, {"before\x1dignored", "before\x1dignored", KeyNone},
	} {
		for split := 0; split <= len(tc.input); split++ {
			var k Keys
			var events []KeyEvent
			events = append(events, k.Feed([]byte(tc.input[:split]))...)
			detached := false
			for _, e := range events {
				detached = detached || e.Action == KeyDetach
			}
			if !detached {
				events = append(events, k.Feed([]byte(tc.input[split:]))...)
			}
			var data []byte
			var actions []KeyAction
			for _, e := range events {
				data = append(data, e.Data...)
				if e.Action != KeyNone {
					actions = append(actions, e.Action)
				}
			}
			if string(data) != tc.want {
				t.Fatalf("%q split %d: %q", tc.input, split, data)
			}
			if tc.action == KeyNone && len(actions) != 0 || tc.action != KeyNone && !reflect.DeepEqual(actions, []KeyAction{tc.action}) {
				t.Fatalf("%q split %d: %v", tc.input, split, actions)
			}
		}
	}
}
func TestBracketedPasteNeverExecutesKeys(t *testing.T) {
	input := []byte("\x1b[200~paste \x02d\x02?\x1d\x02\x02 \x1b[201~\x02i")
	for split := 0; split <= len(input); split++ {
		var k Keys
		data, actions := collectKeys(&k, input[:split], input[split:])
		if !bytes.Equal(data, input[:len(input)-2]) || !reflect.DeepEqual(actions, []KeyAction{KeyInfo}) {
			t.Fatalf("split %d: data=%q actions=%v", split, data, actions)
		}
	}
	var k Keys
	var chunks [][]byte
	for i := range input {
		chunks = append(chunks, input[i:i+1])
	}
	data, actions := collectKeys(&k, chunks...)
	if !bytes.Equal(data, input[:len(input)-2]) || !reflect.DeepEqual(actions, []KeyAction{KeyInfo}) {
		t.Fatal("bytewise paste changed")
	}
}
func TestEscapeAndBypassAreImmediate(t *testing.T) {
	for _, input := range []string{"\x1b", "\x1b[", "\x1b[A", "\x1b[201~", "\x1b\x1b[200~text\x1b[201~"} {
		var k Keys
		data, actions := collectKeys(&k, []byte(input))
		if string(data) != input || len(actions) != 0 {
			t.Fatalf("changed escape input %q: %q %v", input, data, actions)
		}
	}
	k := Keys{Bypass: true}
	input := []byte("\x02d\x1d\x02?")
	data, actions := collectKeys(&k, input)
	if !bytes.Equal(data, input) || len(actions) != 0 {
		t.Fatal("bypass intercepted input")
	}
}
func TestKeyEventsPreserveOrder(t *testing.T) {
	var k Keys
	events := k.Feed([]byte("before\x02iafter\x02dignored"))
	want := []KeyEvent{{Data: []byte("before")}, {Action: KeyInfo}, {Data: []byte("after")}, {Action: KeyDetach}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("got %#v", events)
	}
}

func TestPasteWithEmbeddedControlsAndMissingClose(t *testing.T) {
	// Like the upstream fragmented-paste cases, payload controls stay literal.
	// A missing close must not turn a slow/large paste into local commands.
	payload := bytes.Repeat([]byte("text\x1b]11;rgb:0/0/0\x1b\\\x02d\x1b[20x"), 4096)
	var k Keys
	data, actions := collectKeys(&k, []byte("\x1b[200~"), payload)
	want := append([]byte("\x1b[200~"), payload...)
	if !bytes.Equal(data, want) || len(actions) != 0 {
		t.Fatal("unterminated paste interpreted shortcuts or changed payload")
	}
	if len(k.marker) > 6 {
		t.Fatal("paste delimiter state grew with payload")
	}
	data, actions = collectKeys(&k, []byte("\x1b[20"), []byte("1~\x02i"))
	if string(data) != "\x1b[201~" || !reflect.DeepEqual(actions, []KeyAction{KeyInfo}) {
		t.Fatalf("closing fragmented paste did not restore shortcuts: %q %v", data, actions)
	}
}

func TestPasteCompletionBoundary(t *testing.T) {
	// tmux regress/tty-keys-paste.sh: a separate Escape before paste,
	// fragmented end, then immediately resume normal binding dispatch.
	var k Keys
	data, actions := collectKeys(&k, []byte("\x1b\x1b[200~\x02d\x1b[20"))
	if string(data) != "\x1b\x1b[200~\x02d\x1b[20" || len(actions) != 0 {
		t.Fatal("partial end marker changed paste dispatch")
	}
	data, actions = collectKeys(&k, []byte("1~\x02i"))
	if string(data) != "1~" || !reflect.DeepEqual(actions, []KeyAction{KeyInfo}) {
		t.Fatalf("first post-paste shortcut lost: %q %v", data, actions)
	}
}

func TestSessionOrderAndCycle(t *testing.T) {
	names := []string{"z", "a", "a"}
	if got := SessionOrder("m", names); !reflect.DeepEqual(got, []string{"a", "m", "z"}) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(names, []string{"z", "a", "a"}) {
		t.Fatal("mutated input")
	}
	for _, tc := range []struct {
		current string
		step    int
		want    string
	}{{"m", 1, "z"}, {"z", 1, "a"}, {"a", -1, "z"}} {
		if got := AdjacentSession(tc.current, names, tc.step); got != tc.want {
			t.Fatalf("got %s", got)
		}
	}
	if AdjacentSession("only", nil, 1) != "only" {
		t.Fatal("single session moved")
	}
}
func TestSessionKeys(t *testing.T) {
	for _, key := range []byte{'c', 'n', 'p', 'l'} {
		var k Keys
		events := k.Feed([]byte{2, key, 'x'})
		if len(events) != 1 || !events[0].Action.IsSessionCommand() {
			t.Fatalf("%c: %#v", key, events)
		}
		var paste Keys
		data, actions := collectKeys(&paste, []byte("\x1b[200~"), []byte{2, key}, []byte("\x1b[201~"))
		if len(actions) != 0 || len(data) == 0 {
			t.Fatalf("paste intercepted %c", key)
		}
	}
}

func TestInterruptDetectionAcrossReads(t *testing.T) {
	var keys Keys
	keys.Feed([]byte("typed\x03"))
	if !keys.Interrupted {
		t.Fatal("batched interrupt lost")
	}
	keys.Feed([]byte("\x1b[200"))
	keys.Feed([]byte("~pasted\x03"))
	if keys.Interrupted {
		t.Fatal("paste counted as interrupt")
	}
	keys.Feed([]byte("\x1b[201~\x03\x03"))
	if !keys.Interrupted {
		t.Fatal("interrupt after paste lost")
	}
}
