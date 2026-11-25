package pty

import (
	"crypto/rand"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openConsole keeps the pollable ends for the daemon. ConPTY duplicates the
// other two handles during creation, so they can be closed before spawning.
func (t *Terminal) openConsole() error {
	consoleInput, input, err := consolePipe(false)
	if err != nil {
		return err
	}
	t.input = input
	defer windows.CloseHandle(consoleInput)
	consoleOutput, output, err := consolePipe(true)
	if err != nil {
		return err
	}
	t.output = output
	defer windows.CloseHandle(consoleOutput)
	return windows.CreatePseudoConsole(windows.Coord{X: int16(t.size.Cols), Y: int16(t.size.Rows)}, consoleInput, consoleOutput, 0, &t.console)
}

func (t *Terminal) startProcess(path string, c Config) error {
	application, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{path}, c.Args...)))
	if err != nil {
		return err
	}
	var dir *uint16
	if c.Dir != "" {
		dir, err = windows.UTF16PtrFromString(c.Dir)
		if err != nil {
			return err
		}
	}
	block, err := terminalEnvironment(c.Env)
	if err != nil {
		return err
	}
	var env *uint16
	if len(block) != 0 {
		env = &block[0]
	}
	t.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return err
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	// The attribute takes the opaque HPCON value, not the address of its slot.
	// Reinterpret the native handle without a uintptr-to-Go-pointer conversion.
	consolePointer := *(*unsafe.Pointer)(unsafe.Pointer(&t.console))
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, consolePointer, unsafe.Sizeof(t.console)); err != nil {
		return err
	}
	// Assign the job atomically with process creation, before any child code runs.
	const jobListAttribute = 0x0002000D // PROC_THREAD_ATTRIBUTE_JOB_LIST
	if err = attrs.Update(jobListAttribute, unsafe.Pointer(&t.job), unsafe.Sizeof(t.job)); err != nil {
		return err
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
	err = windows.CreateProcess(application, line, nil, nil, false, flags, env, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(block)
	runtime.KeepAlive(attrs)
	if err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	t.pid = int(pi.ProcessId)
	t.waitResult = sync.OnceValue(func() error { return t.wait(pi.Process) })
	return nil
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

// Return the synchronous ConPTY handle and the daemon's pollable end.
func consolePipe(daemonReads bool) (windows.Handle, *os.File, error) {
	name, err := windows.UTF16PtrFromString(`\\.\pipe\sshore-conpty-` + rand.Text())
	if err != nil {
		return 0, nil, err
	}
	readMode := uint32(windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_FIRST_PIPE_INSTANCE)
	writeFlags := uint32(windows.SECURITY_SQOS_PRESENT | windows.SECURITY_IDENTIFICATION)
	if daemonReads {
		readMode |= windows.FILE_FLAG_OVERLAPPED
	} else {
		writeFlags |= windows.FILE_FLAG_OVERLAPPED
	}
	r, err := windows.CreateNamedPipe(name, readMode,
		windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 0, 64*1024, 0, nil)
	if err != nil {
		return 0, nil, err
	}
	// CreateFile connects to the single server instance before returning.
	w, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, writeFlags, 0)
	if err != nil {
		_ = windows.CloseHandle(r)
		return 0, nil, err
	}
	console, daemon := r, w
	if daemonReads {
		console, daemon = w, r
	}
	// After the startup pipe closes, Windows may reuse its stdin handle value.
	// os.NewFile skips overlapped detection for that original value.
	if daemon == windows.Handle(syscall.Stdin) {
		current := windows.CurrentProcess()
		var duplicate windows.Handle
		err := windows.DuplicateHandle(current, daemon, current, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS)
		_ = windows.CloseHandle(daemon)
		if err != nil {
			_ = windows.CloseHandle(console)
			return 0, nil, err
		}
		daemon = duplicate
	}
	pollable := os.NewFile(uintptr(daemon), "conpty-daemon")
	if err := pollable.SetDeadline(time.Time{}); err != nil {
		_ = pollable.Close()
		_ = windows.CloseHandle(console)
		return 0, nil, fmt.Errorf("enable ConPTY pipe deadlines: %w", err)
	}
	return console, pollable, nil
}
