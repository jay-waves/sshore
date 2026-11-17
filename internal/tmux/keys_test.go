package tmux

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
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
		{"a\x02\x02b", "a\x02b", KeyNone}, {"before\x02dignored", "before", KeyDetach}, {"a\x02rZ", "aZ", KeyRedraw}, {"\x02i", "\x02i", KeyNone}, {"\x02?", "\x02?", KeyNone}, {"\x02x", "\x02x", KeyNone}, {"\x02s", "\x02s", KeyNone}, {"\x02w", "\x02w", KeyNone}, {"before\x1dignored", "before\x1dignored", KeyNone},
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
	input := []byte("\x1b[200~paste \x02d\x02?\x1d\x02\x02 \x1b[201~\x02r")
	for split := 0; split <= len(input); split++ {
		var k Keys
		data, actions := collectKeys(&k, input[:split], input[split:])
		if !bytes.Equal(data, input[:len(input)-2]) || !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
			t.Fatalf("split %d: data=%q actions=%v", split, data, actions)
		}
	}
	var k Keys
	var chunks [][]byte
	for i := range input {
		chunks = append(chunks, input[i:i+1])
	}
	data, actions := collectKeys(&k, chunks...)
	if !bytes.Equal(data, input[:len(input)-2]) || !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
		t.Fatal("bytewise paste changed")
	}
}
func TestEscapeTimeoutAndImmediateBypass(t *testing.T) {
	for _, input := range []string{"\x1b", "\x1b[", "\x1b[A", "\x1b[201~", "\x1b\x1b[200~text\x1b[201~"} {
		var k Keys
		data, actions := collectKeys(&k, []byte(input))
		for _, e := range k.FlushPending() {
			data = append(data, e.Data...)
		}
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
	events := k.Feed([]byte("before\x02rafter\x02dignored"))
	want := []KeyEvent{{Data: []byte("before")}, {Action: KeyRedraw}, {Data: []byte("after")}, {Action: KeyDetach}}
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
	data, actions = collectKeys(&k, []byte("\x1b[20"), []byte("1~\x02r"))
	if string(data) != "\x1b[201~" || !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
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
	data, actions = collectKeys(&k, []byte("1~\x02r"))
	if string(data) != "1~" || !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
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

func TestEnhancedPrefixProtocols(t *testing.T) {
	for _, prefix := range []string{"\x02", "\x1b[98;5u", "\x1b[98;5:1u", "\x1b[98;69u", "\x1b[27;5;98~", "\x1b[1080::98;5u"} {
		for command, action := range map[byte]KeyAction{'c': KeyNew, 'd': KeyDetach, 'n': KeyNext, 'p': KeyPrevious, 'l': KeyLast, 'r': KeyRedraw} {
			for _, next := range []string{string(command), fmt.Sprintf("\x1b[%d;1u", command), fmt.Sprintf("\x1b[27;1;%d~", command)} {
				input := prefix + next
				for split := 0; split <= len(input); split++ {
					var k Keys
					data, actions := collectKeys(&k, []byte(input[:split]), []byte(input[split:]))
					if len(data) != 0 || !reflect.DeepEqual(actions, []KeyAction{action}) {
						t.Fatalf("%q split %d: %q %v", input, split, data, actions)
					}
				}
			}
		}
	}
}

func TestEnhancedReleaseAndRepeat(t *testing.T) {
	for _, prefix := range []string{"\x02", "\x1b[98;5:1u"} {
		input := prefix + "\x1b[98;5:2u\x1b[98;5:3u\x1b[114;1:1u\x1b[114;1:2u\x1b[114;1:3uX"
		var k Keys
		var chunks [][]byte
		for i := range input {
			chunks = append(chunks, []byte(input[i:i+1]))
		}
		data, actions := collectKeys(&k, chunks...)
		if string(data) != "X" || !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
			t.Fatalf("%q %v", data, actions)
		}
	}
}

func TestEnhancedTransparentInput(t *testing.T) {
	for _, input := range []string{
		"你好\x1b[97;5u\x1b[97;5:3u",                                                                 // Unconsumed key events remain byte-identical.
		"\x1b[98;7ud",                                                                              // Ctrl+Alt+B is not the prefix.
		"\x1b[98;5:9ud", "\x1b[999999999999999999999999999998;5ud", "\x1b[?98;5ud", "\x1b[98;5$ud", // Malformed/unknown reports.
		"\x1b]title\x02d\x07", "\x1b]title\x1b[98;5ud\x07", "\x1bP\x1b[98;5ud\x1b\\", "\x1b[M\x02d!", "\x1bPpayload\x02d\x1b\\", // Opaque strings.
		"\x1b[200~\x1b[98;5ud\x03\x1b[201~", // Paste.
	} {
		for split := 0; split <= len(input); split++ {
			var k Keys
			data, actions := collectKeys(&k, []byte(input[:split]), []byte(input[split:]))
			for _, e := range k.FlushPending() {
				data = append(data, e.Data...)
			}
			if string(data) != input || len(actions) != 0 {
				t.Fatalf("%q split %d: %q %v", input, split, data, actions)
			}
		}
	}
	for _, prefix := range []string{"\x02", "\x1b[98;5u", "\x1b[27;5;98~"} {
		var k Keys
		data, actions := collectKeys(&k, []byte(prefix+"\x1b[120;2u"))
		if string(data) != prefix+"\x1b[120;2u" || len(actions) != 0 {
			t.Fatalf("fallback changed %q: %q %v", prefix, data, actions)
		}
		k = Keys{}
		data, actions = collectKeys(&k, []byte(prefix+prefix))
		if string(data) != prefix || len(actions) != 0 {
			t.Fatalf("double prefix changed %q: %q %v", prefix, data, actions)
		}
	}
}

func TestEnhancedInterrupt(t *testing.T) {
	for _, input := range []string{"\x03", "\x1b[99;5u", "\x1b[99;5:2u", "\x1b[27;5;99~"} {
		var k Keys
		for i := range input {
			k.Feed([]byte(input[i : i+1]))
		}
		if !k.Interrupted {
			t.Fatalf("missed interrupt %q", input)
		}
		k.Feed([]byte("\x1b[99;5:3u"))
		if k.Interrupted {
			t.Fatal("release counted as interrupt")
		}
		k.Feed([]byte("\x1b[200~" + input + "\x1b[201~"))
		if k.Interrupted {
			t.Fatal("paste counted as interrupt")
		}
	}
}

func TestKeySequenceTimeoutAndBounds(t *testing.T) {
	for _, input := range []string{"\x1b", "\x1b[", "\x02\x1b[98;"} {
		var k Keys
		k.Feed([]byte(input))
		if !k.Pending() {
			t.Fatalf("no timeout armed for %q", input)
		}
		events := k.FlushPending()
		if len(events) != 1 || string(events[0].Data) != input || k.Pending() {
			t.Fatalf("timeout changed %q: %+v", input, events)
		}
		data, actions := collectKeys(&k, []byte("5ud"))
		if string(data) != "5ud" || len(actions) != 0 {
			t.Fatalf("late tail reinterpreted: %q %v", data, actions)
		}
	}
	var k Keys
	input := "\x1b[" + strings.Repeat("1;", 1000) + "98;5ud"
	data, actions := collectKeys(&k, []byte(input))
	if string(data) != input || len(actions) != 0 || len(k.sequence) > 128 {
		t.Fatal("overlong report changed or retained")
	}
	k = Keys{Bypass: true}
	input = "\x1b[98;5ud\x1b[99;5u"
	data, actions = collectKeys(&k, []byte(input))
	if string(data) != input || len(actions) != 0 || k.Pending() {
		t.Fatal("enhanced bypass changed input")
	}
}

func TestPrefixReleaseFallbackPreservesOrder(t *testing.T) {
	input := "\x1b[98;5u\x1b[98;5:3u\x1b[57442;1:3u\x1b[120;1u"
	var k Keys
	data, actions := collectKeys(&k, []byte(input))
	if string(data) != input || len(actions) != 0 {
		t.Fatalf("release reordered: %q %v", data, actions)
	}
}

func TestEscapeTimeoutAllowsNextShortcut(t *testing.T) {
	var k Keys
	k.Feed([]byte{27})
	k.FlushPending()
	_, actions := collectKeys(&k, []byte{2, 'r'})
	if !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) {
		t.Fatalf("Escape swallowed next shortcut: %v", actions)
	}
}

func TestDeliveredKeysKeepRepeatAndRelease(t *testing.T) {
	for _, input := range []string{
		"\x1b[98;5u\x1b[98;5u\x1b[98;5:2u\x1b[98;5:3u",
		"\x02r\x1b[114;1u\x1b[114;1:2u\x1b[114;1:3u",
	} {
		var k Keys
		data, actions := collectKeys(&k, []byte(input))
		if strings.HasPrefix(input, "\x02") {
			if !reflect.DeepEqual(actions, []KeyAction{KeyRedraw}) || string(data) != input[2:] {
				t.Fatalf("ordinary key events lost: %q %v", data, actions)
			}
		} else if len(actions) != 0 || string(data) != input[len("\x1b[98;5u"):] {
			t.Fatalf("double-prefix events lost: %q %v", data, actions)
		}
	}
}
