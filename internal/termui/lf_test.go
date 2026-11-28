package termui

import (
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/files/dirlist"
)

func TestLFShowsHostBeforePathAndHighlightsCurrentEntry(t *testing.T) {
	var out strings.Builder
	panes := [3]lfPane{{}, {entries: []dirlist.Entry{{Name: "src", IsDir: true}}}, {}}
	var display lfScreen
	if err := display.draw(&out, LFOptions{Host: "work", Title: "Open"}, "/project", panes, -1, 0, 14, 5, nil, true, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	header := display.rows
	if len(header) < 3 || ansi.StringWidth(header[0]) > 13 || !strings.Contains(ansi.Strip(header[0]), "work") || header[1] != "/project" || header[2] != "" {
		t.Fatalf("narrow header = %q", header)
	}
	_, highlighted, start := strings.Cut(out.String(), "\x1b[7m")
	row, _, end := strings.Cut(highlighted, "\x1b[27m")
	if !start || !end || strings.TrimSpace(row) != "src/" || ansi.StringWidth(row) != 5 {
		t.Fatal("reverse video should highlight just the selected name and its trailing space")
	}
}

func TestLFRepaintsOnlyChangedRowsWithinTheViewport(t *testing.T) {
	var display lfScreen
	var out strings.Builder
	var panes [3]lfPane
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		panes[1].entries = append(panes[1].entries, dirlist.Entry{Name: name})
	}
	draw := func(selected int) string {
		t.Helper()
		out.Reset()
		if err := display.draw(&out, LFOptions{Title: "Choose file"}, "/", panes, -1, selected, 80, 7, nil, false, false, nil, ""); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	draw(3)
	update := draw(2)
	if strings.Count(update, ";1H") != 2 || !strings.Contains(update, "\x1b[5;1H") ||
		!strings.Contains(update, "\x1b[6;1H") || strings.Contains(update, "\x1b[2J") || display.firstVisible != 1 {
		t.Fatalf("moving within the viewport repainted unexpected rows: %q", update)
	}
	if update := draw(2); update != "" {
		t.Fatalf("unchanged screen wrote %q", update)
	}
}

func TestLFReusesPreviewWhenEnteringAndReturning(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 1)
	var mu sync.Mutex
	counts := map[string]int{}
	step := 0
	var directory string
	_, err := PickLF(ctx, LFOptions{
		Dir: "/root", Title: "Pick", Input: input,
		Join: func(a, b string) string { return path.Join(a, b) }, Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]dirlist.Entry, error) {
			mu.Lock()
			counts[dir]++
			mu.Unlock()
			switch dir {
			case "/root":
				return []dirlist.Entry{{Name: "child", IsDir: true}}, nil
			case "/root/child":
				return []dirlist.Entry{{Name: "preview-file"}}, nil
			}
			return nil, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case strings.Contains(frame, "\x1b[2;1H/root\x1b[K"):
				directory = "/root"
			case strings.Contains(frame, "\x1b[2;1H/root/child\x1b[K"):
				directory = "/root/child"
			}
			if !strings.Contains(frame, "preview-file") {
				return
			}
			switch {
			case step == 0 && directory == "/root":
				step++
				input <- KeyInput{Data: []byte("l")}
			case step == 1 && directory == "/root/child":
				step++
				input <- KeyInput{Data: []byte("h")}
			case step == 2 && directory == "/root":
				step++
				input <- KeyInput{Data: []byte("q")}
			}
		}),
	})
	if step != 3 || !errors.Is(err, context.Canceled) {
		t.Fatalf("navigation step %d, error %v", step, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["/root"] != 1 || counts["/root/child"] != 1 {
		t.Fatalf("directory reads: %v", counts)
	}
}

func TestLFOpenUsesDirectoryFirstOrdering(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	input := make(chan KeyInput, 1)
	go func() {
		select {
		case <-ready:
			input <- KeyInput{Data: []byte("jl")}
		case <-ctx.Done():
		}
	}()
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Input: input,
		Output: lfTestOutput(func(frame string) {
			if strings.Contains(frame, "\x1b[7mfolder/") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}),
		Join:   func(dir, name string) string { return path.Join(dir, name) },
		Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]dirlist.Entry, error) {
			if dir == "/" {
				return []dirlist.Entry{{Name: "z"}, {Name: "folder", IsDir: true}, {Name: "a"}}, nil
			}
			return nil, nil
		},
	})
	if err != nil || selected != "/a" {
		t.Fatalf("selected %q, error %v", selected, err)
	}
}

type lfTestOutput func(string)

func (out lfTestOutput) Write(data []byte) (int, error) {
	out(string(data))
	return len(data), nil
}
func TestLFLoadsCurrentAndParentTogether(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := make(chan [2]string, 1)
	singles := make(chan string, 2)
	input := make(chan KeyInput, 1)
	done := make(chan error, 1)
	go func() {
		_, err := PickLF(ctx, LFOptions{
			Dir: "/start", Input: input, Output: io.Discard,
			Join:   func(dir, name string) string { return path.Join(dir, name) },
			Parent: path.Dir,
			ReadDir: func(_ context.Context, dir string) ([]dirlist.Entry, error) {
				singles <- dir
				return nil, nil
			},
			ReadPair: func(_ context.Context, current, parent string) ([]dirlist.Entry, error, []dirlist.Entry, error) {
				calls <- [2]string{current, parent}
				return nil, nil, nil, nil
			},
		})
		done <- err
	}()
	select {
	case pair := <-calls:
		if pair != [2]string{"/start", "/"} {
			t.Fatalf("directory pair = %q", pair)
		}
	case dir := <-singles:
		t.Fatalf("unexpected separate read of %q", dir)
	case <-ctx.Done():
		t.Fatal("picker did not request the directory pair")
	}
	input <- KeyInput{Data: []byte{3}}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel picker: %v", err)
	}
}

func TestLFCancelDoesNotChooseAFolder(t *testing.T) {
	for _, key := range []struct {
		name  string
		bytes string
	}{
		{"q", "q"},
		{"Ctrl-C", "\x03"},
	} {
		t.Run(key.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			input := make(chan KeyInput, 1)
			stopped := make(chan struct{})
			selected, err := PickLF(ctx, LFOptions{
				Dir: "/", Directory: true, Input: input, Output: io.Discard,
				Join:   func(dir, name string) string { return path.Join(dir, name) },
				Parent: path.Dir,
				ReadDir: func(ctx context.Context, _ string) ([]dirlist.Entry, error) {
					input <- KeyInput{Data: []byte(key.bytes)}
					<-ctx.Done()
					close(stopped)
					return nil, ctx.Err()
				},
				Validate: func(dir string) error {
					t.Errorf("cancel validated %q", dir)
					return nil
				},
			})
			if selected != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("selected %q, error %v", selected, err)
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("directory read did not stop when the picker closed")
			}
		})
	}
}

func TestLFArrowsReturnFocusToTheDirectoryJustLeft(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 4)
	step := 0
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Title: "Pick", Directory: true, Input: input,
		Join:   func(dir, name string) string { return path.Join(dir, name) },
		Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]dirlist.Entry, error) {
			if dir == "/" {
				return []dirlist.Entry{{Name: "first", IsDir: true}, {Name: "second", IsDir: true}}, nil
			}
			return nil, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- KeyInput{Data: []byte("\x1b[1;5B\x1b[")}
				input <- KeyInput{Data: []byte("B\x1b[B\x1bOC")}
			case step == 1 && strings.Contains(frame, "\x1b[2;1H/second\x1b[K"):
				step++
				input <- KeyInput{Data: []byte("\x1b[D")}
			case step == 2 && strings.Contains(frame, "\x1b[2;1H/\x1b[K") && strings.Contains(frame, "\x1b[7msecond/"):
				step++
				input <- KeyInput{Data: []byte("\x1b[C")}
			case step == 3 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- KeyInput{Data: []byte("\r")}
			}
		}),
	})
	if err != nil || selected != "/second" {
		t.Fatalf("navigation step %d, selected %q, error %v", step, selected, err)
	}
}

func TestLFMarksFilesAcrossDirectories(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 1)
	step := 0
	selected, err := PickLFFiles(ctx, LFOptions{
		Dir: "/", Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]dirlist.Entry, error) {
			if dir == "/" {
				return []dirlist.Entry{{Name: "sub", IsDir: true}, {Name: "a"}, {Name: "b"}}, nil
			}
			return []dirlist.Entry{{Name: "c"}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7msub/"):
				step++
				input <- KeyInput{Data: []byte("j  ")}
			case step == 1 && strings.Contains(ansi.Strip(frame), "▌ a ") && strings.Contains(ansi.Strip(frame), "▌ b ") && strings.Contains(frame, "2 files marked  Enter Continue"):
				step++
				input <- KeyInput{Data: []byte("kkl")}
			case step == 2 && strings.Contains(frame, "\x1b[7mc"):
				step++
				input <- KeyInput{Data: []byte(" \r")}
			}
		}),
	}, nil)
	if err != nil || !slices.Equal(selected, []string{"/a", "/b", "/sub/c"}) {
		t.Fatalf("selected %q, error %v", selected, err)
	}
}

func TestLFJumpsToTopAndBottom(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 2)
	step := 0
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Directory: true, Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(context.Context, string) ([]dirlist.Entry, error) {
			return []dirlist.Entry{{Name: "first", IsDir: true}, {Name: "last", IsDir: true}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- KeyInput{Data: []byte("G")}
			case step == 1 && strings.Contains(frame, "\x1b[7mlast/"):
				step++
				input <- KeyInput{Data: []byte("g")}
				input <- KeyInput{Data: []byte("g")}
			case step == 2 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- KeyInput{Data: []byte("\r")}
			}
		}),
	})
	if err != nil || selected != "/" || step != 3 {
		t.Fatalf("step %d, selected %q, error %v", step, selected, err)
	}
}

func TestLFSearchRepeatsInOriginalDirection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 1)
	step := 0
	names := []string{"0-alpha", "B-report", "C-report"}
	frames := []string{"0-alpha", "C-report", "B-report", "C-report", "B-report"}
	keys := []string{"?report\r", "n", "N", "/report\r", "\r"}
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(context.Context, string) ([]dirlist.Entry, error) {
			var entries []dirlist.Entry
			for _, name := range names {
				entries = append(entries, dirlist.Entry{Name: name})
			}
			return entries, nil
		},
		Output: lfTestOutput(func(frame string) {
			if step < len(keys) && strings.Contains(frame, "\x1b[7m"+frames[step]+" ") {
				input <- KeyInput{Data: []byte(keys[step])}
				step++
			}
		}),
	})
	if err != nil || selected != "/B-report" || step != len(keys) {
		t.Fatalf("step %d, selected %q, error %v", step, selected, err)
	}
}

func TestLFPasteStaysSearchText(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 3)
	step := 0
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(context.Context, string) ([]dirlist.Entry, error) {
			return []dirlist.Entry{{Name: "a"}, {Name: "报告.txt"}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7ma "):
				step++
				input <- KeyInput{Data: []byte(pasteStart + "q\r" + pasteEnd)}
				input <- KeyInput{Data: []byte("/" + pasteStart + "报")}
				input <- KeyInput{Data: []byte("告\r" + pasteEnd[:3])}
			case step == 1 && strings.Contains(frame, "/报告"):
				step++
				input <- KeyInput{Data: []byte(pasteEnd[3:] + "\r\n")}
			case step == 2 && strings.Contains(frame, "\x1b[7m报告.txt "):
				step++
				input <- KeyInput{Data: []byte("\r")}
			}
		}),
	})
	if err != nil || selected != "/报告.txt" || step != 3 {
		t.Fatalf("step %d, selected %q, error %v", step, selected, err)
	}
}

func TestLFEscapeClearsMarksAndQQuits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan KeyInput, 1)
	step := 0
	_, err := PickLFFiles(ctx, LFOptions{
		Dir: "/", Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(context.Context, string) ([]dirlist.Entry, error) {
			return []dirlist.Entry{{Name: "file.txt"}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7mfile.txt"):
				step++
				input <- KeyInput{Data: []byte(" ")}
			case step == 1 && strings.Contains(frame, "1 file marked"):
				step++
				input <- KeyInput{Data: []byte("\x1b")}
			case step == 2 && strings.Contains(frame, "Enter Choose file"):
				step++
				input <- KeyInput{Data: []byte("\x1bq")}
			}
		}),
	}, nil)
	if step != 3 || !errors.Is(err, context.Canceled) {
		t.Fatalf("step %d, error %v", step, err)
	}
}
