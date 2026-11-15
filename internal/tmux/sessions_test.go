package tmux

import (
	"reflect"
	"testing"
)

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
func TestSessionChooser(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		done        bool
	}{{"2\r", "b", true}, {"\x1b", "", true}, {"\x03", "", true}, {"3\r", "", false}, {"12\x7f\r", "a", true}, {"1", "", false}} {
		c := SessionChooser{Names: []string{"a", "b"}}
		got, done := c.Feed([]byte(tc.input))
		if got != tc.want || done != tc.done {
			t.Fatalf("%q: %q %v", tc.input, got, done)
		}
	}
}
func TestSessionKeys(t *testing.T) {
	for _, key := range []byte{'c', 'n', 'p', 'l', 's', 'w'} {
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
