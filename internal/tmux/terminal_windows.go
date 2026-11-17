package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type Terminal struct {
	input, output *os.File
	console       windows.Handle
	process       windows.Handle
	job           windows.Handle
	pid           int
	once          sync.Once
	mu            sync.Mutex
	closed        bool
	size          Size
	waitResult    func() error
}

func Start(c Config) (*Terminal, error) {
	if err := c.Size.validate(); err != nil {
		return nil, err
	}
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	args := []string{syscall.EscapeArg(path)}
	for _, a := range c.Args {
		args = append(args, syscall.EscapeArg(a))
	}
	line, err := windows.UTF16FromString(strings.Join(args, " "))
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if c.Dir != "" {
		dir, err = windows.UTF16PtrFromString(c.Dir)
		if err != nil {
			return nil, err
		}
	}
	var env *uint16
	var block []uint16
	if c.Env != nil {
		items := make([]string, 0, len(c.Env)+1)
		seen := make(map[string]bool)
		for i := len(c.Env) - 1; i >= 0; i-- {
			value := c.Env[i]
			offset := 0
			if strings.HasPrefix(value, "=") {
				offset = 1
			}
			split := strings.IndexByte(value[offset:], '=')
			if split < 0 {
				return nil, fmt.Errorf("invalid terminal environment entry")
			}
			key := strings.ToUpper(value[:offset+split])
			if !seen[key] {
				items = append(items, value)
				seen[key] = true
			}
		}
		if !seen["SYSTEMROOT"] {
			items = append(items, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
		}
		sort.SliceStable(items, func(i, j int) bool { return strings.ToUpper(items[i]) < strings.ToUpper(items[j]) })
		for _, v := range items {
			if v == "" {
				return nil, fmt.Errorf("empty terminal environment entry")
			}
			u, e := windows.UTF16FromString(v)
			if e != nil {
				return nil, e
			}
			block = append(block, u...)
		}
		block = append(block, 0)
		if len(block) == 1 {
			block = append(block, 0)
		}
		env = &block[0]
	}
	inR, inW, err := consolePipe()
	if err != nil {
		return nil, err
	}
	defer inR.Close()
	outR, outW, err := consolePipe()
	if err != nil {
		inW.Close()
		return nil, err
	}
	defer outW.Close()
	t := &Terminal{input: inW, output: outR, size: c.Size}
	t.waitResult = sync.OnceValue(t.wait)
	t.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Close()
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		t.Close()
		return nil, err
	}
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(c.Size.Cols), Y: int16(c.Size.Rows)}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &t.console); err != nil {
		t.Close()
		return nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		t.Close()
		return nil, err
	}
	defer attrs.Delete()
	// The attribute takes the opaque HPCON value, not the address of its slot.
	// Reinterpret the native handle without a uintptr-to-Go-pointer conversion.
	consolePointer := *(*unsafe.Pointer)(unsafe.Pointer(&t.console))
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, consolePointer, unsafe.Sizeof(t.console)); err != nil {
		t.Close()
		return nil, err
	}
	// Assign the job atomically with process creation, before any child code runs.
	const jobListAttribute = 0x0002000D // PROC_THREAD_ATTRIBUTE_JOB_LIST
	if err = attrs.Update(jobListAttribute, unsafe.Pointer(&t.job), unsafe.Sizeof(t.job)); err != nil {
		t.Close()
		return nil, err
	}
	si := windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	// Do not inherit the worker's redirected (possibly already closed) startup
	// pipes. ConPTY must supply the child's console standard handles.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = windows.InvalidHandle
	si.StdOutput = windows.InvalidHandle
	si.StdErr = windows.InvalidHandle
	si.ProcThreadAttributeList = attrs.List()
	var pi windows.ProcessInformation
	application, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Close()
		return nil, err
	}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	err = windows.CreateProcess(application, &line[0], nil, nil, false, flags, env, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(block)
	runtime.KeepAlive(attrs)
	if err != nil {
		t.Close()
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	t.process = pi.Process
	t.pid = int(pi.ProcessId)
	// ConPTY keeps its output pipe open after the client process exits. Reap
	// independently so closing the console can finish the active output reader.
	go t.Wait()
	return t, nil
}
func (t *Terminal) Read(p []byte) (int, error)  { return t.output.Read(p) }
func (t *Terminal) Write(p []byte) (int, error) { return t.input.Write(p) }
func (t *Terminal) PID() int                    { return t.pid }
func (t *Terminal) Resize(s Size) error {
	if err := s.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
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
func (t *Terminal) wait() error {
	defer windows.CloseHandle(t.process)
	defer t.closeConsole()
	if _, err := windows.WaitForSingleObject(t.process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(t.process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("terminal process exited with code %d", code)
	}
	return nil
}

// consolePipe requests bounded buffering without making handles inheritable.
func consolePipe() (*os.File, *os.File, error) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 64*1024); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(read), "conpty-read"), os.NewFile(uintptr(write), "conpty-write"), nil
}
func (t *Terminal) Close() error {
	t.once.Do(func() {
		t.input.Close()
		// Close output before ConPTY so shutdown never depends on a reader.
		t.output.Close()
		t.closeConsole()
	})
	return nil
}

func (t *Terminal) closeConsole() {
	t.mu.Lock()
	t.closed = true
	h, job := t.console, t.job
	t.console, t.job = 0, 0
	t.mu.Unlock()
	if job != 0 {
		windows.CloseHandle(job)
	}
	if h != 0 {
		windows.ClosePseudoConsole(h)
	}
}

// A temporary size change asks ConPTY to repaint without injecting input.
func RequestRedraw(t *Terminal, _ int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.size.Cols < 2 {
		return
	}
	changed := windows.Coord{X: int16(t.size.Cols - 1), Y: int16(t.size.Rows)}
	if windows.ResizePseudoConsole(t.console, changed) == nil {
		original := t.size
		t.size.Cols--
		// Track the actual size even if restoration fails, so the next Resize
		// retries instead of incorrectly treating the original size as current.
		if windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(original.Cols), Y: int16(original.Rows)}) == nil {
			t.size = original
		}
	}
}
