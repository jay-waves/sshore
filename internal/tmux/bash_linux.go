//go:build linux

package tmux

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const bashOutputLimit = 1 << 20

// Bash runs complete scripts in one noninteractive process. Callers own its
// lifecycle and must serialize submissions.
type Bash struct {
	cmd        *exec.Cmd
	input      io.WriteCloser
	status     *bufio.Reader
	statusFile *os.File
	dir        string
	mu         sync.Mutex
	stateMu    sync.Mutex
	closed     bool
	broken     bool
	finished   chan struct{}
	waitErr    error
}

// BashExecution exposes foreground completion separately from stream closure.
type BashExecution struct {
	Done       chan struct{}
	OutputDone chan struct{}
	mu         sync.Mutex
	result     BashResult
	err        error
	changed    chan struct{}
}
type BashResult struct {
	Stdout, Stderr                    []byte
	ExitCode                          int
	Complete, OutputClosed, Truncated bool
}

func (e *BashExecution) Result() (BashResult, error) {
	r, err, _ := e.Snapshot()
	return r, err
}

// Snapshot returns a notification channel that closes when the result changes.
func (e *BashExecution) Snapshot() (BashResult, error, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.result
	r.Stdout = append([]byte(nil), r.Stdout...)
	r.Stderr = append([]byte(nil), r.Stderr...)
	return r, e.err, e.changed
}
func (e *BashExecution) notify() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func StartBash() (*Bash, error) {
	dir, err := os.MkdirTemp("", "sshore-bash-")
	if err != nil {
		return nil, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.ExtraFiles = []*os.File{controlWrite}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		controlRead.Close()
		controlWrite.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = null, null
	input, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	controlWrite.Close()
	null.Close()
	if err != nil {
		controlRead.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	b := &Bash{cmd: cmd, input: input, status: bufio.NewReader(controlRead), statusFile: controlRead, dir: dir, finished: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		// Descendants may inherit the descriptor after Bash exits.
		_ = controlRead.Close()
		b.stateMu.Lock()
		b.waitErr = err
		b.stateMu.Unlock()
		close(b.finished)
	}()
	return b, nil
}

func bashQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Run executes a script in the current Bash process. Background descendants can
// keep output streams open; their output remains associated with this call.
func (b *Bash) Run(code, stdin string) (*BashExecution, error) {
	if !b.mu.TryLock() {
		return nil, fmt.Errorf("bash session is busy")
	}
	var result *BashExecution
	unlock := true
	defer func() {
		if unlock {
			b.mu.Unlock()
		}
	}()
	b.stateMu.Lock()
	closed := b.closed || b.broken
	b.stateMu.Unlock()
	if closed || !b.Alive() {
		return result, fmt.Errorf("bash session closed")
	}
	if strings.ContainsRune(code, 0) {
		return result, fmt.Errorf("NUL in bash code")
	}
	id := rand.Text()
	path := filepath.Join(b.dir, id)
	if err := os.Mkdir(path, 0700); err != nil {
		return result, err
	}
	retain := false
	defer func() {
		if !retain {
			_ = os.RemoveAll(path)
		}
	}()
	codePath, inPath := filepath.Join(path, "code"), filepath.Join(path, "stdin")
	outPath, errPath := filepath.Join(path, "stdout"), filepath.Join(path, "stderr")
	if err := os.WriteFile(codePath, []byte(code), 0600); err != nil {
		return result, err
	}
	if err := os.WriteFile(inPath, []byte(stdin), 0600); err != nil {
		return result, err
	}
	if err := syscall.Mkfifo(outPath, 0600); err != nil {
		return result, err
	}
	if err := syscall.Mkfifo(errPath, 0600); err != nil {
		return result, err
	}

	execution := &BashExecution{Done: make(chan struct{}), OutputDone: make(chan struct{}), changed: make(chan struct{})}
	type streamResult struct{ err error }
	read := func(path string, isError bool, ch chan<- streamResult) {
		f, err := os.Open(path)
		if err != nil {
			ch <- streamResult{err}
			return
		}
		defer f.Close()
		buf := make([]byte, 32*1024)
		for {
			n, e := f.Read(buf)
			if n > 0 {
				execution.mu.Lock()
				dst := &execution.result.Stdout
				if isError {
					dst = &execution.result.Stderr
				}
				remaining := bashOutputLimit - len(*dst)
				if n > remaining {
					execution.result.Truncated = true
				}
				if remaining > 0 {
					*dst = append(*dst, buf[:min(n, remaining)]...)
				}
				execution.notify()
				execution.mu.Unlock()
			}
			if e != nil {
				if errors.Is(e, io.EOF) {
					e = nil
				}
				ch <- streamResult{e}
				return
			}
		}
	}
	out, stderr := make(chan streamResult, 1), make(chan streamResult, 1)
	command := fmt.Sprintf("builtin source %s <%s >%s 2>%s; builtin printf '%%s %%s\\n' %s \"$?\" >&3\n", bashQuote(codePath), bashQuote(inPath), bashQuote(outPath), bashQuote(errPath), bashQuote(id))
	if _, err := io.WriteString(b.input, command); err != nil {
		return result, err
	}
	go read(outPath, false, out)
	go read(errPath, true, stderr)
	retain = true

	unlock = false
	go func() {
		defer b.mu.Unlock()
		line, err := b.status.ReadSlice('\n')
		fields := strings.Fields(string(line))
		code := 0
		if err == nil && (len(fields) != 2 || fields[0] != id) {
			err = fmt.Errorf("invalid bash completion")
		}
		if err == nil {
			code, err = strconv.Atoi(fields[1])
		}
		execution.mu.Lock()
		if err != nil {
			execution.err = fmt.Errorf("bash completion unavailable: %w", err)
		} else {
			execution.result.Complete = true
			execution.result.ExitCode = code
		}
		execution.notify()
		execution.mu.Unlock()
		if err != nil {
			b.stateMu.Lock()
			b.broken = true
			b.stateMu.Unlock()
			// Unblock FIFO readers if Bash exited before opening either stream.
			for _, name := range []string{outPath, errPath} {
				if f, e := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0); e == nil {
					f.Close()
				}
			}
		}
		close(execution.Done)
	}()
	go func() {
		o, e := <-out, <-stderr
		execution.mu.Lock()
		execution.result.OutputClosed = o.err == nil && e.err == nil
		if o.err != nil || e.err != nil {
			execution.err = errors.Join(execution.err, fmt.Errorf("output read failed: %v %v", o.err, e.err))
		}
		execution.notify()
		execution.mu.Unlock()
		close(execution.OutputDone)
		_ = os.RemoveAll(path)
	}()
	return execution, nil
}

func (b *Bash) Close() error {
	b.stateMu.Lock()
	if b.closed {
		b.stateMu.Unlock()
		return nil
	}
	b.closed = true
	b.stateMu.Unlock()
	_ = b.input.Close()
	_ = b.cmd.Process.Kill()
	_ = b.statusFile.Close()
	<-b.finished
	b.stateMu.Lock()
	err := b.waitErr
	b.stateMu.Unlock()
	_ = os.RemoveAll(b.dir)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// PID identifies the owned Bash process for the application lifecycle.
func (b *Bash) PID() int { return b.cmd.Process.Pid }

// Alive reports whether the Bash process is still running.
func (b *Bash) Alive() bool {
	select {
	case <-b.finished:
		return false
	default:
		return true
	}
}

// Usable reports whether the process and its control channel can accept work.
func (b *Bash) Usable() bool {
	b.stateMu.Lock()
	usable := !b.closed && !b.broken
	b.stateMu.Unlock()
	return usable && b.Alive()
}
