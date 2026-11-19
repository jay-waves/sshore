package tmux

import (
	"sync"

	"golang.org/x/sys/windows"
)

var (
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	createThreadpoolWait       = kernel32.NewProc("CreateThreadpoolWait")
	setThreadpoolWait          = kernel32.NewProc("SetThreadpoolWait")
	waitForThreadpoolCallbacks = kernel32.NewProc("WaitForThreadpoolWaitCallbacks")
	closeThreadpoolWait        = kernel32.NewProc("CloseThreadpoolWait")
	processWaits               sync.Map // Native wait pointer → Go completion channel.
)

var processWaitCallback = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(_, _ uintptr, wait uintptr, _ uintptr) uintptr {
		done, _ := processWaits.Load(wait)
		close(done.(chan struct{}))
		return 0
	})
})

func waitForProcessExit(process windows.Handle) error {
	wait, _, _ := createThreadpoolWait.Call(processWaitCallback(), 0, 0)
	if wait == 0 {
		// Keep reaping a live child if Windows cannot allocate a wait object.
		_, err := windows.WaitForSingleObject(process, windows.INFINITE)
		return err
	}
	done := make(chan struct{})
	processWaits.Store(wait, done)
	setThreadpoolWait.Call(wait, uintptr(process), 0)
	<-done
	// Disarm and join the callback before releasing either native handle.
	setThreadpoolWait.Call(wait, 0, 0)
	waitForThreadpoolCallbacks.Call(wait, 0)
	processWaits.Delete(wait)
	closeThreadpoolWait.Call(wait)
	return nil
}
