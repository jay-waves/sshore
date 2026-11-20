package benchmarks

import (
	"strings"
	"testing"

	"github.com/jay-waves/sshore/internal/pty"
)

func BenchmarkTerminalInputObserve(b *testing.B) {
	for _, tc := range []struct{ name, data string }{
		{"plain", strings.Repeat("build output: ordinary UTF-8 中文 text\r\n", 180)},
		{"styled", strings.Repeat("\x1b[32mbuild output\x1b[0m ordinary text\r\n", 180)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := []byte(tc.data)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			var state pty.InputState
			for b.Loop() {
				state.Observe(data)
			}
		})
	}
}
