package term

import (
	"bytes"
	"io"
	"path/filepath"
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
		want := "\x1b[?1002h\x1b[?1016h"
		if got != want {
			t.Fatalf("split %d: %q", split, got)
		}
	}
}

func TestWorkingDirectoryAcrossReadBoundaries(t *testing.T) {
	path, err := filepath.Abs("dir ; 目录")
	if err != nil {
		t.Fatal(err)
	}
	for _, end := range []string{"\a", "\x1b\\"} {
		input := "\x1b]9;9;\"" + path + "\"" + end
		for split := 0; split < len(input); split++ {
			var state InputState
			var report DirectoryReport
			if got := report.Observe(&state, []byte(input[:split])); got != "" {
				t.Fatalf("split %d: partial report returned %q", split, got)
			}
			if got := report.Observe(&state, []byte(input[split:])); got != path {
				t.Fatalf("split %d: report %q, want %q", split, got, path)
			}
		}
	}
}

func TestWorkingDirectoryIgnoresInvalidReports(t *testing.T) {
	path, err := filepath.Abs("work")
	if err != nil {
		t.Fatal(err)
	}
	var state InputState
	input := "\x1b]9;9;" + path + "\a" +
		"\x1b]9;9;relative\a" +
		"\x1b]9;9;" + path + "\x18\a" +
		"\x1bP\x1b]9;9;" + path + "other\x1b\\" +
		"\x1b]9;9;" + path + "\xff\a" +
		"\x1b[?2004h"
	var report DirectoryReport
	if got := report.Observe(&state, []byte(input)); got != path {
		t.Fatalf("report %q, want %q", got, path)
	}
	if !bytes.Contains(state.Replay(), []byte("\x1b[?2004h")) {
		t.Fatal("lost input mode after OSC")
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

func TestReplayModifyOtherKeysBeforePartialControl(t *testing.T) {
	const input = "\x1b[>4;2m\x1b[?2004"
	var state InputState
	state.Observe([]byte(input))
	if got := string(state.Replay()); got != input {
		t.Fatalf("replay %q, want %q", got, input)
	}
	var out bytes.Buffer
	if err := state.EndConsole(&out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), finishTerminalOutput+"\x1b[<1u\x1b[?1049l"+inputModeBaseline+input; got != want {
		t.Fatalf("console restore %q, want %q", got, want)
	}
}

func TestRestoreApplicationModes(t *testing.T) {
	const app = "\x1b[?1049h\x1b[?1003h\x1b[?1003s\x1b[?1002h\x1b[?2004s\x1b[?2004h\x1b=\x1b[>4;2m"
	const continuation = "\x1b[?1003;2004r"
	var application, attached, console InputState
	application.Observe([]byte(app + continuation[:7]))
	attached.Observe(application.Replay())
	console.Observe([]byte(app + continuation[:7]))
	var transitions bytes.Buffer
	if err := application.BeginConsole(&transitions); err != nil {
		t.Fatal(err)
	}
	// Simulate a local editor that changes mouse reporting and keypad mode.
	transitions.WriteString("\x1b[?1000h\x1b>")
	if err := application.EndConsole(&transitions); err != nil {
		t.Fatal(err)
	}
	console.Observe(transitions.Bytes())
	for _, restored := range []*InputState{&application, &attached, &console} {
		restored.Observe([]byte(continuation[7:]))
		if restored.modes != application.modes {
			t.Fatalf("restored modes %#v differ from application modes %#v", restored.modes, application.modes)
		}
	}
	if got := string(application.Replay()); strings.Contains(got, "\x1b[?2004h") ||
		!strings.Contains(got, "\x1b[?1003h") || strings.Contains(got, "\x1b[?1002h") {
		t.Fatalf("saved modes not restored by the application: %q", got)
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
			if !sameEnabledModes(&resumed, &original) || resumed.MouseEnabled() {
				t.Fatalf("kind %c split %d: opaque payload became input modes", kind, split)
			}
		}
	}
}

func TestBoundaryCleanupAfterTruncatedControl(t *testing.T) {
	for _, prefix := range []string{"", "\x1b", "\x1b[?1003;", "\x1b]title", "\x1bPpayload", "\x1b_payload\x1b"} {
		t.Run(prefix, func(t *testing.T) {
			var state InputState
			state.Observe([]byte("\x1b[?1003;1006;1004;2004;2031;2033;2048h" + prefix))
			// Exercise arbitrary transport boundaries, including CAN and ST.
			for _, c := range []byte(terminalInputResetSequence()) {
				state.Observe([]byte{c})
			}
			for i, enabled := range state.modes.enabled {
				if enabled {
					t.Fatalf("mode %d survived cleanup", inputModes[i])
				}
			}
			state.Observe([]byte("\x1b[?1003h"))
			if !state.MouseEnabled() {
				t.Fatal("cleanup left parser inside a control string")
			}
		})
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
		{"common terminal sequences", "\x1b]7;file://host/path\x1b\\\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\x1b]133;A\a\x1b]9;4;1;50\a\x1b[?2026htext\x1b[?2026l", "\x1b]7;file://host/path\x1b\\\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\x1b]133;A\a\x1b]9;4;1;50\a\x1b[?2026htext\x1b[?2026l"},
		{"opaque strings", "\x1b]2;\x1b[?9001h\a\x1bP\x1b[?9001l\x1b\\", "\x1b]2;\x1b[?9001h\a\x1bP\x1b[?9001l\x1b\\"},
		{"partial", "text\x1b[?900", "text\x1b[?900"},
		{"aborted", "\x1b[?900\x1b[?9001hA", "\x1b[?900\x18A"},
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
func sameEnabledModes(a, b *InputState) bool {
	return a.modes.enabled == b.modes.enabled
}
