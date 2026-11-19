//go:build linux

package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const bashOutputLimit = 1 << 20
const bashExecutionLimit = 16

// BashSession owns the noninteractive shell and its bounded execution history.
type BashSession struct {
	cmd         *exec.Cmd
	input       io.Writer
	status      *bufio.Reader
	dir         string
	mu          sync.Mutex
	unavailable atomic.Bool
	stop        sync.Once
	finished    chan struct{}
	waitErr     error
	executions  []*bashExecution
}

// bashExecution exposes foreground completion separately from stream closure.
type bashExecution struct {
	ID         string
	Done       chan struct{}
	OutputDone chan struct{}
	mu         sync.Mutex
	result     bashResult
	changed    chan struct{}
}
type bashResult struct {
	Stdout, Stderr                    []byte
	ExitCode                          int
	Complete, OutputClosed, Truncated bool
	Err                               error
}

// readOutput copies a bounded chunk from each stream after at most one wait
// for an observable change. Only waiting readers need a notification channel.
func (e *bashExecution) readOutput(ctx context.Context, stdout, stderr, limit int, wait time.Duration) (bashResult, error) {
	for {
		r, changed, err := e.outputChunk(stdout, stderr, limit, wait > 0)
		if changed == nil {
			return r, err
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return bashResult{}, ctx.Err()
		case <-changed:
		case <-timer.C:
		}
		wait = 0
	}
}

func (e *bashExecution) outputChunk(stdout, stderr, limit int, wait bool) (bashResult, <-chan struct{}, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.result
	if stdout < 0 || stdout > len(r.Stdout) || stderr < 0 || stderr > len(r.Stderr) || limit < 0 {
		return bashResult{}, nil, fmt.Errorf("invalid output cursor")
	}
	out, errOut := r.Stdout[stdout:], r.Stderr[stderr:]
	var changed <-chan struct{}
	if wait && len(out) == 0 && len(errOut) == 0 && !r.OutputClosed && r.Err == nil {
		if e.changed == nil {
			e.changed = make(chan struct{})
		}
		changed = e.changed
	}
	r.Stdout = slices.Clone(out[:min(len(out), limit)])
	r.Stderr = slices.Clone(errOut[:min(len(errOut), limit)])
	r.OutputClosed = r.OutputClosed && len(r.Stdout) == len(out) && len(r.Stderr) == len(errOut)
	return r, changed, nil
}
func (e *bashExecution) notify() {
	if e.changed != nil {
		close(e.changed)
		e.changed = nil
	}
}

func StartBashSession(ctx context.Context) (result *BashSession, _ error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sshore-bash-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			_ = os.RemoveAll(dir)
		}
	}()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer controlWrite.Close()
	defer func() {
		if result == nil {
			_ = controlRead.Close()
		}
	}()
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.ExtraFiles = []*os.File{controlWrite}
	input, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return nil, err
	}
	b := &BashSession{cmd: cmd, input: input, status: bufio.NewReader(controlRead), dir: dir, finished: make(chan struct{})}
	go func() {
		b.waitErr = cmd.Wait()
		b.unavailable.Store(true)
		// Descendants may inherit the descriptor after Bash exits.
		_ = controlRead.Close()
		// Let any submission settle before removing its temporary paths.
		b.mu.Lock()
		if n := len(b.executions); n != 0 {
			<-b.executions[n-1].Done
		}
		_ = os.RemoveAll(dir)
		b.mu.Unlock()
		close(b.finished)
	}()
	return b, nil
}

func bashQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// run executes a script in the current Bash process. Background descendants can
// keep output streams open; their output remains associated with this call.
func (b *BashSession) run(ctx context.Context, code, stdin string) (*bashExecution, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	oldest := -1
	if len(b.executions) == bashExecutionLimit {
		oldest = slices.IndexFunc(b.executions, func(e *bashExecution) bool {
			return channelClosed(e.Done) && channelClosed(e.OutputDone)
		})
		if oldest < 0 {
			return nil, fmt.Errorf("execution limit reached; output streams are still open")
		}
	}
	if n := len(b.executions); n > 0 && !channelClosed(b.executions[n-1].Done) {
		return nil, fmt.Errorf("bash session is busy")
	}
	if b.unavailable.Load() {
		return nil, fmt.Errorf("bash session closed")
	}
	if strings.ContainsRune(code, 0) {
		return nil, fmt.Errorf("NUL in bash code")
	}
	id := rand.Text()
	path := filepath.Join(b.dir, id)
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	var readers [2]*os.File
	submitted := false
	defer func() {
		if submitted {
			return
		}
		for _, reader := range readers {
			if reader != nil {
				_ = reader.Close()
			}
		}
		_ = os.RemoveAll(path)
	}()
	codePath, inPath := filepath.Join(path, "code"), filepath.Join(path, "stdin")
	outPath, errPath := filepath.Join(path, "stdout"), filepath.Join(path, "stderr")
	if err := os.WriteFile(codePath, []byte(code), 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(inPath, []byte(stdin), 0600); err != nil {
		return nil, err
	}
	for i, name := range [...]string{outPath, errPath} {
		if err := syscall.Mkfifo(name, 0600); err != nil {
			return nil, err
		}
		// Open readers before submission so Bash can open both writers.
		// A nonblocking open also lets us clean up if Bash never opens them.
		reader, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		readers[i] = reader
	}

	execution := &bashExecution{ID: id, Done: make(chan struct{}), OutputDone: make(chan struct{})}
	read := func(f *os.File, dst *[]byte, stream string) error {
		defer f.Close()
		buf := make([]byte, 32*1024)
		for {
			n, e := f.Read(buf)
			if n > 0 {
				execution.mu.Lock()
				remaining := bashOutputLimit - len(*dst)
				changed := remaining > 0 || !execution.result.Truncated
				if n > remaining {
					execution.result.Truncated = true
				}
				if remaining > 0 {
					*dst = append(*dst, buf[:min(n, remaining)]...)
				}
				if changed {
					execution.notify()
				}
				execution.mu.Unlock()
			}
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return fmt.Errorf("%s: %w", stream, e)
			}
		}
	}
	command := fmt.Sprintf("{ builtin printf '%%s ready\\n' %s >&3; builtin source %s; } <%s >%s 2>%s; builtin printf '%%s %%s\\n' %s \"$?\" >&3\n",
		bashQuote(id), bashQuote(codePath), bashQuote(inPath), bashQuote(outPath), bashQuote(errPath), bashQuote(id))
	if _, err := io.WriteString(b.input, command); err != nil {
		// Bash may have read part of the command. A later submission
		// cannot safely reuse this input stream.
		b.unavailable.Store(true)
		return nil, err
	}
	submitted = true
	if oldest >= 0 {
		b.executions = slices.Delete(b.executions, oldest, oldest+1)
	}
	b.executions = append(b.executions, execution)
	go func() {
		line, err := b.status.ReadSlice('\n')
		// Before this marker, a FIFO with no writers can look like EOF.
		// A missing marker means setup failed or Bash exited. The already
		// opened readers can still drain any output and finish.
		go func() {
			var stdout sync.WaitGroup
			var stdoutErr error
			stdout.Go(func() {
				stdoutErr = read(readers[0], &execution.result.Stdout, "stdout")
			})
			stderrErr := read(readers[1], &execution.result.Stderr, "stderr")
			stdout.Wait()
			outputErr := errors.Join(stdoutErr, stderrErr)
			execution.mu.Lock()
			execution.result.OutputClosed = outputErr == nil
			if outputErr != nil {
				execution.result.Err = errors.Join(execution.result.Err, fmt.Errorf("output read failed: %w", outputErr))
			}
			close(execution.OutputDone)
			execution.notify()
			execution.mu.Unlock()
			_ = os.RemoveAll(path)
		}()
		if err == nil && string(line) == id+" ready\n" {
			line, err = b.status.ReadSlice('\n')
		}
		fields := strings.Fields(string(line))
		code := 0
		if err == nil && (len(fields) != 2 || fields[0] != id) {
			err = fmt.Errorf("invalid bash completion")
		}
		if err == nil {
			code, err = strconv.Atoi(fields[1])
		}
		if err != nil {
			b.unavailable.Store(true)
		}
		execution.mu.Lock()
		if err != nil {
			execution.result.Err = errors.Join(execution.result.Err, fmt.Errorf("bash completion unavailable: %w", err))
		} else {
			execution.result.Complete = true
			execution.result.ExitCode = code
		}
		close(execution.Done) // Admit the next script before notifying output readers.
		execution.notify()
		execution.mu.Unlock()
	}()
	return execution, nil
}

func (b *BashSession) Close() error {
	b.stop.Do(func() {
		b.unavailable.Store(true)
		_ = b.cmd.Process.Kill()
	})
	<-b.finished
	err := b.waitErr
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil
	}
	return err
}

// PID identifies the owned Bash process for the application lifecycle.
func (b *BashSession) PID() int { return b.cmd.Process.Pid }

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
