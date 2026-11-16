package tmux

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestBoundaryCleanupAfterTruncatedControl(t *testing.T) {
	for _, prefix := range []string{"", "\x1b", "\x1b[?1003;", "\x1b]title", "\x1bPpayload", "\x1b_payload\x1b"} {
		t.Run(prefix, func(t *testing.T) {
			var state InputState
			state.Observe([]byte("\x1b[?1003;1006;1004;2004;2031;2033;2048h" + prefix))
			// Exercise arbitrary transport boundaries, including CAN and ST.
			for _, c := range []byte(terminalInputResetSequence()) {
				state.Observe([]byte{c})
			}
			for i, enabled := range state.enabled {
				if enabled {
					t.Fatalf("mode %s survived cleanup", inputModes[i])
				}
			}
			state.Observe([]byte("\x1b[?1003h"))
			if !state.MouseEnabled() {
				t.Fatal("cleanup left parser inside a control string")
			}
		})
	}
}

func TestBoundaryCleanupRestoresPromptDisplay(t *testing.T) {
	seq := terminalInputResetSequence()
	for _, control := range []string{"\x1b[?2026l", "\x1b]8;;\x1b\\", "\x1b[0m", "\x1b[?25h"} {
		if !strings.Contains(seq, control) {
			t.Errorf("missing prompt cleanup %q", control)
		}
	}
	if !strings.HasPrefix(seq, "\x18\x1b\\") {
		t.Fatal("cleanup must cancel incomplete controls first")
	}
}

func TestConsoleInputPreservesControlKeysAndUnicode(t *testing.T) {
	batches := [][]uint16{{'A', 0x1a, 3, 0x1d, 0x4e2d, 0xd83d}, {0xde80, 'Z'}}
	r := &consoleInput{readUnits: func(b []uint16) (int, error) {
		if len(batches) == 0 {
			return 0, io.EOF
		}
		n := copy(b, batches[0])
		batches = batches[1:]
		return n, nil
	}}
	var got bytes.Buffer
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		got.Write(one[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := "A\x1a\x03\x1d\u4e2d\U0001f680Z"; got.String() != want {
		t.Fatalf("got %q want %q", got.String(), want)
	}
}
func TestConsoleInputTruncatedSurrogate(t *testing.T) {
	once := false
	r := &consoleInput{readUnits: func(b []uint16) (int, error) {
		if once {
			return 0, io.EOF
		}
		once = true
		b[0] = 0xd83d
		return 1, nil
	}}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "\ufffd" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestConsoleOutputModes(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"win32", "a\x1b[?9001hb\x1b[?9001lc", "abc"},
		{"embedded controls", "\x1b[\a?90\r01\x7fh", "\a\r\x7f"},
		{"controls combined", "\x1b[?9001;20\a04h", "\a\x1b[?2004h"},
		{"other controls unchanged", "\x1b[?20\a04h", "\x1b[?20\a04h"},
		{"cancelled request", "\x1b[?9001\x18h", "\x1b[?9001\x18h"},
		{"combined", "\x1b[?1;09001;1006h\x1b[?9001;2004l", "\x1b[?1;1006h\x1b[?2004l"},
		{"other modes", "\x1b[?1004h\x1b[?2004h\x1b[?1l\x1b[2J\x1b[31m中文😀", "\x1b[?1004h\x1b[?2004h\x1b[?1l\x1b[2J\x1b[31m中文😀"},
		{"opaque strings", "\x1b]2;\x1b[?9001h\a\x1bP\x1b[?9001l\x1b\\", "\x1b]2;\x1b[?9001h\a\x1bP\x1b[?9001l\x1b\\"},
		{"partial", "text\x1b[?900", "text\x1b[?900"},
		{"aborted", "\x1b[?900\x1b[?9001h", "\x1b[?900"},
		{"bounded", "\x1b[?" + strings.Repeat("1;", 100) + "h", "\x1b[?" + strings.Repeat("1;", 100) + "h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for size := 1; size <= len(tc.input); size++ {
				var out bytes.Buffer
				w := &consoleOutput{out: &out}
				for start := 0; start < len(tc.input); start += size {
					chunk := tc.input[start:min(start+size, len(tc.input))]
					if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
						t.Fatalf("write: %d, %v", n, err)
					}
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if out.String() != tc.want {
					t.Fatalf("chunk size %d: got %q want %q", size, out.String(), tc.want)
				}
			}
		})
	}
}
