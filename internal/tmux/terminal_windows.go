package tmux

import (
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

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
}

func Start(c Config) (result *Terminal, _ error) {
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
	application, err := windows.UTF16PtrFromString(path)
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
	block, err := terminalEnvironment(c.Env)
	if err != nil {
		return nil, err
	}
	var env *uint16
	if len(block) != 0 {
		env = &block[0]
	}
	t := &Terminal{size: c.Size}
	// Release the child-facing pipe ends before rollback or starting the waiter.
	defer func() {
		if result == nil {
			_ = t.Close()
			return
		}
		// ConPTY keeps its output pipe open after the child exits.
		go t.Wait()
	}()
	inR, inW, err := consoleInputPipe()
	if err != nil {
		return nil, err
	}
	t.input = inW
	defer inR.Close()
	outR, outW, err := consolePipe()
	if err != nil {
		return nil, err
	}
	t.output = outR
	defer outW.Close()
	t.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(c.Size.Cols), Y: int16(c.Size.Rows)}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &t.console); err != nil {
		return nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// The attribute takes the opaque HPCON value, not the address of its slot.
	// Reinterpret the native handle without a uintptr-to-Go-pointer conversion.
	consolePointer := *(*unsafe.Pointer)(unsafe.Pointer(&t.console))
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, consolePointer, unsafe.Sizeof(t.console)); err != nil {
		return nil, err
	}
	// Assign the job atomically with process creation, before any child code runs.
	const jobListAttribute = 0x0002000D // PROC_THREAD_ATTRIBUTE_JOB_LIST
	if err = attrs.Update(jobListAttribute, unsafe.Pointer(&t.job), unsafe.Sizeof(t.job)); err != nil {
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
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	err = windows.CreateProcess(application, &line[0], nil, nil, false, flags, env, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(block)
	runtime.KeepAlive(attrs)
	if err != nil {
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	t.pid = int(pi.ProcessId)
	t.waitResult = sync.OnceValue(func() error { return t.wait(pi.Process) })
	return t, nil
}

func terminalEnvironment(env []string) ([]uint16, error) {
	if env == nil {
		return nil, nil
	}
	entries := make(map[string]string, len(env)+1)
	for _, value := range env {
		// A drive's working directory uses a name such as =C:.
		offset := 0
		if strings.HasPrefix(value, "=") {
			offset = 1
		}
		split := strings.IndexByte(value[offset:], '=')
		if split < 0 {
			return nil, fmt.Errorf("invalid terminal environment entry")
		}
		entries[strings.ToUpper(value[:offset+split])] = value
	}
	if _, ok := entries["SYSTEMROOT"]; !ok {
		entries["SYSTEMROOT"] = "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")
	}
	var block []uint16
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry, err := windows.UTF16FromString(entries[name])
		if err != nil {
			return nil, err
		}
		block = append(block, entry...)
	}
	return append(block, 0), nil
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
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("terminal process exited with code %d", code)
	}
	return nil
}

// ConPTY reads synchronously. Only our write end uses overlapped I/O so Go
// can cancel a blocked write by deadline without closing the terminal.
// TODO: On native Windows amd64, run the ConPTY tests and verify Ctrl+B
// detach/reattach during blocked input and final output on shell exit.
func consoleInputPipe() (read, write *os.File, err error) {
	name, err := windows.UTF16PtrFromString(`\\.\pipe\sshore-conpty-` + rand.Text())
	if err != nil {
		return nil, nil, err
	}
	r, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 0, 64*1024, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = windows.CloseHandle(r)
		}
	}()
	w, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, nil, err
	}
	if err = windows.ConnectNamedPipe(r, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		_ = windows.CloseHandle(w)
		return nil, nil, err
	}
	writer := os.NewFile(uintptr(w), "conpty-input")
	if err = writer.SetWriteDeadline(time.Time{}); err != nil {
		_ = writer.Close()
		return nil, nil, err
	}
	return os.NewFile(uintptr(r), "conpty-input-read"), writer, nil
}

// consolePipe requests bounded output buffering without inheritable handles.
func consolePipe() (*os.File, *os.File, error) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 64*1024); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(read), "conpty-read"), os.NewFile(uintptr(write), "conpty-write"), nil
}
func (t *Terminal) Close() error {
	t.closePipes.Do(func() {
		if t.input != nil {
			_ = t.input.Close()
		}
		// Close output before ConPTY so shutdown never depends on a reader.
		if t.output != nil {
			_ = t.output.Close()
		}
	})
	t.closeConsole()
	return nil
}

func (t *Terminal) closeConsole() {
	t.closeHandles.Do(func() {
		t.mu.Lock()
		h, job := t.console, t.job
		t.console, t.job = 0, 0
		t.mu.Unlock()
		if job != 0 {
			windows.CloseHandle(job)
		}
		if h != 0 {
			windows.ClosePseudoConsole(h)
		}
	})
}

// A temporary size change asks ConPTY to repaint without injecting input.
func RequestRedraw(t *Terminal, _ int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.console == 0 || t.size.Cols < 2 {
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
