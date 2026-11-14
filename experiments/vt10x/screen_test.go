package probe

// Compatibility gate for adopting vt10x as sshore's screen observer. Run with
// `cd experiments/vt10x && go test -v`. This isolated module does not change the
// production dependency graph, start PTYs, or touch real sessions. Failures are
// unmet requirements, intentionally not blessed as expected behavior.
// This tests state tracking only, not ANSI restoration or real Vim rendering.

import (
	"testing"

	"github.com/hinshun/vt10x"
)

func feed(t *testing.T, v vt10x.Terminal, s string) {
	t.Helper()
	n, err := v.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write consumed %d/%d bytes: %v", n, len(s), err)
	}
}

func TestASCIIOverwrite(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "progress 10%\r\x1b[2Kdone")
	if v.Cell(0, 0).Char != 'd' || v.Cell(4, 0).Char != ' ' || v.Cursor().X != 4 {
		t.Fatalf("unexpected screen: %q cursor=%+v", v.String(), v.Cursor())
	}
}

func TestAlternateScreen(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "shell\x1b[?1049h\x1b[Heditor")
	if v.Cell(0, 0).Char != 'e' || v.Mode()&vt10x.ModeAltScreen == 0 {
		t.Fatal("alternate screen not entered")
	}
	feed(t, v, "\x1b[?1049l")
	if v.Cell(0, 0).Char != 's' || v.Mode()&vt10x.ModeAltScreen != 0 {
		t.Fatal("main screen not restored")
	}
}

func TestWideCharacter(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "中A")
	if v.Cursor().X != 3 || v.Cell(2, 0).Char != 'A' {
		t.Fatalf("wide character must occupy two columns; cursor X=%d", v.Cursor().X)
	}
}

func TestCombiningCharacter(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "e\u0301A")
	if v.Cursor().X != 2 {
		t.Fatalf("combining accent must not advance cursor; X=%d", v.Cursor().X)
	}
}

func TestInsertMode(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "abc\r\x1b[4hX\x1b[4l")
	if v.Cell(1, 0).Char != 'a' || v.Cell(3, 0).Char != 'c' {
		t.Fatalf("expected Xabc, got %q", v.String())
	}
}

func TestSavedMouseModes(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "\x1b[?1003;1006h\x1b[?1003;1006s\x1b[?1003;1006l\x1b[?1003;1006r")
	want := vt10x.ModeMouseMany | vt10x.ModeMouseSgr
	if v.Mode()&want != want {
		t.Fatalf("saved mouse modes lost: mode=%#x", v.Mode())
	}
}

func TestSplitCSI(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	feed(t, v, "\x1b[2;")
	feed(t, v, "3HX")
	if v.Cell(2, 1).Char != 'X' {
		t.Fatal("split cursor-position sequence lost")
	}
}

func TestSplitUTF8DirectWrites(t *testing.T) {
	v := vt10x.New(vt10x.WithSize(20, 4))
	// PTY reads may end anywhere, including within a UTF-8 encoded rune.
	// A short write here identifies the need for an explicit buffering adapter.
	b := []byte("中")
	feed(t, v, string(b[:2]))
	feed(t, v, string(b[2:]))
	if v.Cell(0, 0).Char != '中' {
		t.Fatalf("UTF-8 split lost: %q", v.String())
	}
}
