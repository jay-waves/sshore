package termui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

type completionFunc func([]rune, int) ([][]rune, int)

func (f completionFunc) Do(line []rune, pos int) ([][]rune, int) { return f(line, pos) }

func TestConsoleWriterCombinesEraseAndRedraw(t *testing.T) {
	var output [][]byte
	w := &consoleWriter{send: func(p []byte) { output = append(output, bytes.Clone(p)) }}
	_, _ = w.Write([]byte("\x1b[J\x1b[2K\r"))
	if len(output) != 0 {
		t.Fatal("erase was displayed separately")
	}
	_, _ = w.Write([]byte("sshore> x"))
	if len(output) != 1 || string(output[0]) != "\x1b[?2026h\x1b[J\x1b[2K\rsshore> x\x1b[?2026l" {
		t.Fatalf("redraw: %q", output)
	}
	_, _ = w.Write([]byte("\x1b[J\x1b[2K\r"))
	w.flush()
	if len(output) != 2 || string(output[1]) != "\x1b[J\x1b[2K\r" {
		t.Fatalf("pending erase: %q", output)
	}
}

func TestConsoleCompletionCommonPrefixAndReset(t *testing.T) {
	c := &consoleCompletion{}
	c.set(completionFunc(func(_ []rune, _ int) ([][]rune, int) {
		return [][]rune{[]rune("目录甲"), []rune("目录乙")}, 2
	}))
	got, offset := c.Do([]rune("前缀"), 2)
	if len(got) != 1 || string(got[0]) != "目录" || offset != 2 {
		t.Fatalf("common prefix %q offset %d", got, offset)
	}
	c.set(completionFunc(func(_ []rune, _ int) ([][]rune, int) {
		return [][]rune{[]rune("甲"), []rune("乙")}, 0
	}))
	line := []rune("前缀")
	c.Do(line, len(line))
	selected, _, ok := c.changed(nil, 0, 0)
	if !ok || string(selected) != "前缀甲" {
		t.Fatalf("first choice: %q", selected)
	}
	c.filter('x')
	c.Do([]rune("前缀x"), len([]rune("前缀x")))
	selected, _, ok = c.changed(nil, 0, 0)
	if !ok || string(selected) != "前缀x甲" {
		t.Fatalf("editing did not reset completion: %q", selected)
	}
}

func TestConsoleCompletionCyclesWithoutListing(t *testing.T) {
	events := make(chan ConsoleEvent, 1024)
	c := StartConsole(context.Background(), "sshore> ", 24, false, func(e ConsoleEvent) { events <- e })
	defer c.Close()
	c.Show("", completionFunc(func(_ []rune, _ int) ([][]rune, int) {
		return [][]rune{[]rune("alpha"), []rune("中文" + strings.Repeat("x", 80))}, 0
	}))
	waitOutput := func(want string) []byte {
		t.Helper()
		var output []byte
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for !bytes.Contains(output, []byte(want)) {
			select {
			case e := <-events:
				if e.Closed {
					t.Fatal("console closed")
				}
				output = append(output, e.Output...)
			case <-timer.C:
				t.Fatalf("missing %q in %q", want, output)
			}
		}
		return output
	}
	waitOutput("sshore> ")
	if !c.TryInput([]byte("x\t")) {
		t.Fatal("input rejected")
	}
	if output := waitOutput("alpha"); bytes.Contains(output, []byte("\n")) {
		t.Fatalf("completion emitted a candidate grid: %q", output)
	}
	if !c.TryInput([]byte("\t")) {
		t.Fatal("second tab rejected")
	}
	want := "中文" + strings.Repeat("x", 80)
	if output := waitOutput(want); bytes.Contains(output, []byte("\n")) {
		t.Fatalf("cycling emitted a candidate grid: %q", output)
	}
	if !c.TryInput([]byte("\r")) {
		t.Fatal("enter rejected")
	}
	for {
		select {
		case e := <-events:
			if e.Line != nil {
				if *e.Line != "x"+want {
					t.Fatalf("submitted %q", *e.Line)
				}
				c.Show("command result", nil)
				output := waitOutput("sshore> ")
				if !bytes.Contains(output, []byte("\rcommand result\r\n")) ||
					bytes.Contains(output, []byte("\x1b[2J")) || bytes.Contains(output, []byte("\x1b[H")) {
					t.Fatalf("expected result to append before the next prompt: %q", output)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("submission timed out")
		}
	}
}

func TestCleanConsoleLine(t *testing.T) {
	input := "\x1b[32mok\x1b[39m \x1b[2J \x1b]0;title\a"
	want := "\x1b[32mok\x1b[39m �[2J �]0;title�"
	if got := cleanConsoleLine(input, true); got != want {
		t.Fatalf("cleanConsoleLine = %q, want %q", got, want)
	}
}
