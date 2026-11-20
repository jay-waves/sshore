package pty

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestConPTYRedraw(t *testing.T) {
	const width, height = 40, 8
	row := func(y, cols int) string {
		return fmt.Sprintf("%02d%s|", y+1, strings.Repeat(string(rune('A'+y)), cols-3))
	}
	start := func(t *testing.T, role, screen string) *Terminal {
		t.Helper()
		term, err := Start(Config{
			Path: os.Args[0], Args: []string{"-test.run=^TestConPTYRedraw$"},
			Env:  append(os.Environ(), "SSHORE_REDRAW_ROLE="+role, "SSHORE_REDRAW_SCREEN="+screen),
			Size: Size{Cols: width, Rows: height},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { term.Close() })
		return term
	}
	screen := os.Getenv("SSHORE_REDRAW_SCREEN")
	switch os.Getenv("SSHORE_REDRAW_ROLE") {
	case "application":
		restore, err := RawConsole()
		if err != nil {
			t.Fatal(err)
		}
		defer restore()
		if screen == "alternate" {
			fmt.Print("\x1b[?1049h")
		}
		lastWidth := 0
		for {
			var info windows.ConsoleScreenBufferInfo
			if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info); err != nil {
				t.Fatal(err)
			}
			cols := int(info.Window.Right - info.Window.Left + 1)
			if cols != lastWidth {
				for y := 0; y < height; y++ {
					fmt.Printf("\x1b[%d;1H%s", y+1, row(y, cols))
				}
				lastWidth = cols
			}
			time.Sleep(10 * time.Millisecond)
		}
	case "client":
		restore, err := RawConsole()
		if err != nil {
			t.Fatal(err)
		}
		defer restore()
		term := start(t, "application", screen)
		out, _ := OutputWriter()
		go io.Copy(out, term)
		readCells := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
		waitForGrid := func(cols int) {
			t.Helper()
			deadline := time.Now().Add(3 * time.Second)
			for {
				for y := 0; y < height; y++ {
					buf := make([]uint16, cols)
					var n uint32
					ok, _, err := readCells.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(cols), uintptr(y<<16), uintptr(unsafe.Pointer(&n)))
					if ok == 0 {
						t.Fatal(err)
					}
					got := windows.UTF16ToString(buf[:n])
					if got != row(y, cols) {
						if time.Now().After(deadline) {
							t.Fatalf("row %d at width %d: got %q, want %q", y, cols, got, row(y, cols))
						}
						break
					}
					if y == height-1 {
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		waitForGrid(width)
		for i := 0; i < 2; i++ {
			// Reattachment and closing the command overlay can both leave a blank display.
			fmt.Print("\x1b[2J\x1b[H")
			RequestRedraw(term)
			waitForGrid(width)
		}
		// Both layers need the same dimensions before testing redraw after a real resize.
		fmt.Print("SSHORE_RESIZE_READY")
		deadline := time.Now().Add(3 * time.Second)
		for {
			var info windows.ConsoleScreenBufferInfo
			if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info); err != nil {
				t.Fatal(err)
			}
			if int(info.Window.Right-info.Window.Left+1) == width+3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("outer resize timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := term.Resize(Size{Cols: width + 3, Rows: height}); err != nil {
			t.Fatal(err)
		}
		waitForGrid(width + 3)
		fmt.Print("\x1b[2J\x1b[H")
		RequestRedraw(term)
		waitForGrid(width + 3)
	default:
		for _, screen := range []string{"main", "alternate"} {
			t.Run(screen, func(t *testing.T) {
				term := start(t, "client", screen)
				done := make(chan error, 1)
				var transcript strings.Builder
				go func() {
					buf := make([]byte, 4096)
					resized := false
					for {
						n, err := term.Read(buf)
						transcript.Write(buf[:n])
						if !resized && strings.Contains(transcript.String(), "SSHORE_RESIZE_READY") {
							resized = true
							if err := term.Resize(Size{Cols: width + 3, Rows: height}); err != nil {
								done <- err
								return
							}
						}
						if err != nil {
							done <- term.Wait()
							return
						}
					}
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%v: %q", err, transcript.String())
					}
					RequestRedraw(term) // A best-effort request can race with process exit.
				case <-time.After(15 * time.Second):
					t.Fatal("nested ConPTY timed out")
				}
			})
		}
	}
}
