package tmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestConPTYRedirectedStartup(t *testing.T) {
	if os.Getenv("SSHORE_TEST_REDIRECTED_CONPTY") == "1" {
		// Match a background worker after its startup pipe handshake finishes.
		os.Stdin.Close()
		os.Stdout.Close()
		if err := checkInteractiveConPTY(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestConPTYRedirectedStartup$")
	cmd.Env = append(os.Environ(), "SSHORE_TEST_REDIRECTED_CONPTY=1")
	cmd.Stdin = strings.NewReader("")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("redirected worker: %v (%v)\n%s", err, ctx.Err(), output.String())
	}
}

func checkInteractiveConPTY() error {
	term, err := Start(Config{Path: "cmd.exe", Args: []string{"/d", "/q"}, Size: Size{Cols: 80, Rows: 24}})
	if err != nil {
		return err
	}
	defer term.Close()
	output := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(term); output <- b }()
	if _, err := term.Write([]byte("echo SSHORE_INTERACTIVE_OK\r\nexit\r\n")); err != nil {
		return err
	}
	b := <-output
	if err := term.Wait(); err != nil {
		return err
	}
	if !strings.Contains(string(b), "SSHORE_INTERACTIVE_OK") {
		return fmt.Errorf("interactive command produced no marker: %q", b)
	}
	return nil
}

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
