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
		{"a\x02\x02b", "a\x02b", KeyNone}, {"before\x02dignored", "before", KeyDetach}, {"a\x02rZ", "aZ", KeyRedraw}, {"\x02i", "", KeyInfo}, {"\x02?", "", KeyHelp}, {"\x02x", "\x02x", KeyNone}, {"before\x1dignored", "before\x1dignored", KeyNone},
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
