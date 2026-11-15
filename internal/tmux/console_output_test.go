package tmux

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsoleOutputModes(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"win32", "a\x1b[?9001hb\x1b[?9001lc", "abc"},
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
