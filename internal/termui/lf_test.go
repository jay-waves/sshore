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
)

func TestLFShowsHostBeforePathAndHighlightsCurrentEntry(t *testing.T) {
	var out strings.Builder
	panes := [3]lfPane{{}, {entries: []LFEntry{{Name: "src", IsDir: true}}}, {}}
	if err := lfDraw(&out, LFOptions{Host: "work", Title: "Open shell"}, "/project", panes, -1, 0, 14, 5, nil, true, false, nil); err != nil {
		t.Fatal(err)
	}
	header := strings.Split(ansi.Strip(out.String()), "\r\n")
	if len(header) < 3 || ansi.StringWidth(header[0]) > 13 || header[1] != "/project" || header[2] != "" {
		t.Fatalf("narrow header = %q", header)
	}
	_, highlighted, start := strings.Cut(out.String(), "\x1b[7m")
	row, _, end := strings.Cut(highlighted, "\x1b[27m")
	if !start || !end || strings.TrimSpace(row) != "src/" || ansi.StringWidth(row) != 13 {
		t.Fatal("the selected directory did not get the full current column's reverse video")
	}
}

func TestLFReusesPreviewWhenEnteringAndReturning(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := make(chan LFInput, 1)
	var mu sync.Mutex
	counts := map[string]int{}
	step := 0
	_, err := PickLF(ctx, LFOptions{
		Dir: "/root", Title: "Pick", Input: input,
		Join: func(a, b string) string { return path.Join(a, b) }, Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]LFEntry, error) {
			mu.Lock()
			counts[dir]++
			mu.Unlock()
			switch dir {
			case "/root":
				return []LFEntry{{Name: "child", IsDir: true}}, nil
			case "/root/child":
				return []LFEntry{{Name: "preview-file"}}, nil
			}
			return nil, nil
		},
		Output: lfTestOutput(func(frame string) {
			if !strings.Contains(frame, "preview-file") {
				return
			}
			switch {
			case step == 0 && strings.Contains(frame, "Pick\r\n/root\r\n"):
				step++
				input <- LFInput{Data: []byte("l")}
			case step == 1 && strings.Contains(frame, "Pick\r\n/root/child\r\n"):
				step++
				input <- LFInput{Data: []byte("h")}
			case step == 2 && strings.Contains(frame, "Pick\r\n/root\r\n"):
				step++
				input <- LFInput{Data: []byte("q")}
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
	input := make(chan LFInput, 1)
	go func() {
		select {
		case <-ready:
			input <- LFInput{Data: []byte("jl")}
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
		ReadDir: func(_ context.Context, dir string) ([]LFEntry, error) {
			if dir == "/" {
				return []LFEntry{{Name: "z"}, {Name: "folder", IsDir: true}, {Name: "a"}}, nil
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
	input := make(chan LFInput, 1)
	done := make(chan error, 1)
	go func() {
		_, err := PickLF(ctx, LFOptions{
			Dir: "/start", Input: input, Output: io.Discard,
			Join:   func(dir, name string) string { return path.Join(dir, name) },
			Parent: path.Dir,
			ReadDir: func(_ context.Context, dir string) ([]LFEntry, error) {
				singles <- dir
				return nil, nil
			},
			ReadPair: func(_ context.Context, current, parent string) ([]LFEntry, error, []LFEntry, error) {
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
	input <- LFInput{Data: []byte{3}}
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
		{"Escape", "\x1b"},
		{"Ctrl-C", "\x03"},
	} {
		t.Run(key.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			input := make(chan LFInput, 1)
			stopped := make(chan struct{})
			selected, err := PickLF(ctx, LFOptions{
				Dir: "/", Directory: true, Input: input, Output: io.Discard,
				Join:   func(dir, name string) string { return path.Join(dir, name) },
				Parent: path.Dir,
				ReadDir: func(ctx context.Context, _ string) ([]LFEntry, error) {
					input <- LFInput{Data: []byte(key.bytes)}
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
	input := make(chan LFInput, 4)
	step := 0
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Title: "Pick", Directory: true, Input: input,
		Join:   func(dir, name string) string { return path.Join(dir, name) },
		Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]LFEntry, error) {
			if dir == "/" {
				return []LFEntry{{Name: "first", IsDir: true}, {Name: "second", IsDir: true}}, nil
			}
			return nil, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- LFInput{Data: []byte("\x1b[1;5B\x1b[")}
				input <- LFInput{Data: []byte("B\x1b[B\x1bOC")}
			case step == 1 && strings.Contains(frame, "Pick\r\n/second\r\n"):
				step++
				input <- LFInput{Data: []byte("\x1b[D")}
			case step == 2 && strings.Contains(frame, "Pick\r\n/\r\n") && strings.Contains(frame, "\x1b[7msecond/"):
				step++
				input <- LFInput{Data: []byte("\x1b[C")}
			case step == 3 && strings.Contains(frame, "Pick\r\n/second\r\n") && !strings.Contains(frame, "…"):
				step++
				input <- LFInput{Data: []byte("\r")}
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
	input := make(chan LFInput, 1)
	step := 0
	selected, err := PickLFFiles(ctx, LFOptions{
		Dir: "/", Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(_ context.Context, dir string) ([]LFEntry, error) {
			if dir == "/" {
				return []LFEntry{{Name: "sub", IsDir: true}, {Name: "a"}, {Name: "b"}}, nil
			}
			return []LFEntry{{Name: "c"}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7msub/"):
				step++
				input <- LFInput{Data: []byte("j  kkl")}
			case step == 1 && strings.Contains(frame, "\x1b[7mc"):
				step++
				input <- LFInput{Data: []byte(" \r")}
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
	input := make(chan LFInput, 2)
	step := 0
	selected, err := PickLF(ctx, LFOptions{
		Dir: "/", Directory: true, Input: input,
		Join: func(dir, name string) string { return path.Join(dir, name) }, Parent: path.Dir,
		ReadDir: func(context.Context, string) ([]LFEntry, error) {
			return []LFEntry{{Name: "first", IsDir: true}, {Name: "last", IsDir: true}}, nil
		},
		Output: lfTestOutput(func(frame string) {
			switch {
			case step == 0 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- LFInput{Data: []byte("G")}
			case step == 1 && strings.Contains(frame, "\x1b[7mlast/"):
				step++
				input <- LFInput{Data: []byte("g")}
				input <- LFInput{Data: []byte("g")}
			case step == 2 && strings.Contains(frame, "\x1b[7m./"):
				step++
				input <- LFInput{Data: []byte("\r")}
			}
		}),
	})
	if err != nil || selected != "/" || step != 3 {
		t.Fatalf("step %d, selected %q, error %v", step, selected, err)
	}
}
