package mux

import (
	"strings"
	"testing"
	"time"
)

func TestConPTY(t *testing.T) {
	term, err := Start(Config{Path: "cmd.exe", Args: []string{"/d", "/c", "echo SSHORE_CONPTY"}, Size: Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	output := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, e := term.Read(buf)
			b.Write(buf[:n])
			if strings.Contains(b.String(), "SSHORE_CONPTY") || e != nil {
				output <- b.String()
				break
			}
		}
		for {
			if _, e := term.Read(buf); e != nil {
				return
			}
		}
	}()
	defer term.Close()
	if err := term.Resize(Size{Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-output:
		if !strings.Contains(s, "SSHORE_CONPTY") {
			t.Fatalf("unexpected output %q", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ConPTY output timed out")
	}
	if err := term.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWithoutReader(t *testing.T) {
	term, err := Start(Config{Path: "cmd.exe", Args: []string{"/d", "/c", "for /L %i in (1,1,100000) do @echo SSHORE_OUTPUT"}, Size: Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { term.Close(); term.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ConPTY shutdown blocked without a reader")
	}
}

func TestInvalidStartupArguments(t *testing.T) {
	for _, c := range []Config{
		{Path: "cmd.exe", Args: []string{"bad\x00arg"}, Size: Size{Cols: 80, Rows: 24}},
		{Path: "cmd.exe", Dir: "bad\x00dir", Size: Size{Cols: 80, Rows: 24}},
		{Path: "cmd.exe", Env: []string{""}, Size: Size{Cols: 80, Rows: 24}},
		{Path: "cmd.exe", Env: []string{"X=bad\x00value"}, Size: Size{Cols: 80, Rows: 24}},
	} {
		if term, err := Start(c); err == nil {
			term.Close()
			term.Wait()
			t.Fatal("invalid startup accepted")
		}
	}
}
