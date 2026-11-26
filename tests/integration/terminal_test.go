//go:build integration && (linux || windows)

package tests_test

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("read %s: %q, %v", path, got, err)
	}
}

// Most markers are complete output lines. Approval prompts have no newline.
type terminalWait func(marker string, prompt ...bool) string

func terminalDialogue(t *testing.T, terminal io.ReadWriter, run func(send func(string), wait terminalWait)) string {
	t.Helper()
	reader := bufio.NewReader(terminal)
	var transcript strings.Builder
	// Prompts can stop drawing without a newline while they wait for input.
	wait := func(marker string, prompt ...bool) string {
		t.Helper()
		for {
			var line strings.Builder
			for {
				char, err := reader.ReadByte()
				if err != nil {
					t.Fatalf("waiting for %q: %v\n%s", marker, err, transcript.String())
				}
				line.WriteByte(char)
				transcript.WriteByte(char)
				if char == '\n' || len(prompt) > 0 && prompt[0] && strings.Contains(ansi.Strip(line.String()), marker) {
					break
				}
			}
			if strings.Contains(ansi.Strip(line.String()), marker) {
				return line.String()
			}
		}
	}
	send := func(input string) {
		t.Helper()
		if _, err := io.WriteString(terminal, input); err != nil {
			t.Fatal(err)
		}
	}
	run(send, wait)
	// Drain any final output so a verbose child cannot block on the terminal.
	_, _ = io.Copy(&transcript, reader)
	return transcript.String()
}
