package pty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type Terminal struct {
	input, output *os.File
	console       windows.Handle
	job           windows.Handle
	pid           int
	closePipes    sync.Once
	closeHandles  sync.Once
	mu            sync.Mutex
	size          Size
	waitResult    func() error
	exited        chan struct{}
	stop          func() error
}

func (c Config) findExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		dir, err := filepath.Abs(c.Dir)
		if err != nil {
			return "", err
		}
		volume := filepath.VolumeName(path)
		switch {
		case volume != "":
			// Drive-relative names use that drive's current directory.
			if !strings.EqualFold(volume, filepath.VolumeName(dir)) {
				dir = c.getenv("=" + volume)
				if dir == "" {
					dir = volume + `\`
				}
			}
			path = filepath.Join(dir, path[len(volume):])
		case os.IsPathSeparator(path[0]):
			path = filepath.VolumeName(dir) + path
		default:
			path = filepath.Join(dir, path)
		}
	}
	isFile := func(name string) bool {
		info, err := os.Stat(name)
		return err == nil && !info.IsDir()
	}
	if filepath.Ext(path) != "" && isFile(path) {
		return path, nil
	}
	extensions := c.getenv("PATHEXT")
	if extensions == "" {
		extensions = ".COM;.EXE;.BAT;.CMD"
	}
	for ext := range strings.SplitSeq(extensions, ";") {
		if ext != "" {
			candidate := path + "." + strings.TrimPrefix(ext, ".")
			if isFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", &exec.Error{Name: path, Err: exec.ErrNotFound}
}

func Start(c Config) (*Terminal, error) {
	if err := c.Size.validate(); err != nil {
		return nil, err
	}
	path, err := c.executablePath()
	if err != nil {
		return nil, err
	}
	t := &Terminal{size: c.Size, exited: make(chan struct{})}
	t.stop = sync.OnceValue(func() error {
		return stopTerminal(t.exited, func() error {
			t.closeIO()
			go t.closePseudoConsole()
			return nil
		}, t.closeJob)
	})
	if err := t.openConsole(); err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("open ConPTY: %w", err)
	}
	if err := t.startProcess(path, c); err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("start terminal process: %w", err)
	}
	// ConPTY keeps its output pipe open after the child exits.
	go t.Wait()
	return t, nil
}

func (t *Terminal) Read(p []byte) (int, error)  { return t.output.Read(p) }
func (t *Terminal) Write(p []byte) (int, error) { return t.input.Write(p) }
func (t *Terminal) PID() int                    { return t.pid }
func (t *Terminal) SetWriteDeadline(at time.Time) error {
	return t.input.SetWriteDeadline(at)
}
func (t *Terminal) Resize(s Size) error {
	if err := s.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.console == 0 {
		return os.ErrClosed
	}
	if t.size == s {
		return nil
	}
	if err := windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(s.Cols), Y: int16(s.Rows)}); err != nil {
		return err
	}
	t.size = s
	return nil
}

// Wait reaps the process once; concurrent callers receive the same result.
func (t *Terminal) Wait() error { return t.waitResult() }
func (t *Terminal) wait(process windows.Handle) error {
	defer windows.CloseHandle(process)
	defer t.closeConsole()
	if err := waitForProcessExit(process); err != nil {
		return err
	}
	close(t.exited)
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return err
	}
	if code != 0 {
		return processExitError(code)
	}
	return nil
}

type processExitError uint32

func (e processExitError) Error() string {
	return fmt.Sprintf("terminal process exited with code %d", e)
}
func (e processExitError) ExitCode() int { return int(e) }

func (t *Terminal) Stop() error { return t.stop() }

func (t *Terminal) Close() error {
	t.closeIO()
	t.closeConsole()
	return nil
}

func (t *Terminal) closeIO() {
	t.closePipes.Do(func() {
		if t.input != nil {
			_ = t.input.Close()
		}
		// Close output before ConPTY so shutdown never depends on a reader.
		if t.output != nil {
			_ = t.output.Close()
		}
	})
}

func (t *Terminal) closeConsole() {
	_ = t.closeJob()
	t.closePseudoConsole()
}

func (t *Terminal) closeJob() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return nil
	}
	if err := windows.CloseHandle(t.job); err != nil {
		return err
	}
	t.job = 0
	return nil
}

func (t *Terminal) closePseudoConsole() {
	t.closeHandles.Do(func() {
		t.mu.Lock()
		h := t.console
		t.console = 0
		t.mu.Unlock()
		if h != 0 {
			windows.ClosePseudoConsole(h)
		}
	})
}

// ConPTY repaints its screen when asked to resize to its current dimensions.
// Changing columns here makes it emit rows wrapped for a different display.
func RequestRedraw(t *Terminal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.console == 0 {
		return
	}
	_ = windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(t.size.Cols), Y: int16(t.size.Rows)})
}
