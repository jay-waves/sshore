package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	shellmux "github.com/jay-waves/sshore/internal/mux"
)

func TestNestedConsoleInput(t *testing.T) {
	if result := os.Getenv("SSHORE_TEST_NESTED_RESULT"); result != "" {
		restore, err := rawTerminal()
		if err != nil {
			t.Fatal(err)
		}
		defer restore()
		// A child ConPTY emits this request during startup. The client currently
		// writes it unchanged to its own console.
		fmt.Print("\x1b[?9001hSSHORE_INPUT_READY")
		chunks := make(chan []byte, 64)
		stop, err := startTerminalKeyboard(context.Background(), func(b []byte, err error) {
			if len(b) > 0 {
				chunks <- b
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		var data []byte
		select {
		case data = <-chunks:
		case <-time.After(5 * time.Second):
			t.Fatal("no input")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case b := <-chunks:
				data = append(data, b...)
			case <-timer.C:
				if err := os.WriteFile(result, data, 0600); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	for _, tc := range []struct{ name, input string }{
		{"vt", "\x1b[A"},
		{"win32", "\x1b[38;72;0;1;256;1_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := t.TempDir() + "/input"
			term, err := shellmux.Start(shellmux.Config{Path: os.Args[0], Args: []string{"-test.run=^TestNestedConsoleInput$"}, Env: append(os.Environ(), "SSHORE_TEST_NESTED_RESULT="+result), Size: shellmux.Size{Cols: 100, Rows: 30}})
			if err != nil {
				t.Fatal(err)
			}
			defer term.Close()
			done := make(chan string, 1)
			go func() {
				var transcript strings.Builder
				buf := make([]byte, 4096)
				sent := false
				for {
					n, err := term.Read(buf)
					transcript.Write(buf[:n])
					if !sent && strings.Contains(transcript.String(), "SSHORE_INPUT_READY") {
						sent = true
						_, _ = io.WriteString(term, tc.input)
					}
					if err != nil {
						done <- transcript.String()
						return
					}
				}
			}()
			select {
			case output := <-done:
				if err := term.Wait(); err != nil {
					t.Fatalf("%v: %q", err, output)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("nested console timeout")
			}
			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("upstream=%q forwarded=%q", tc.input, data)
		})
	}
}
