package proc

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func LiveIdentity(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return "", err
	}
	if state != uint32(windows.WAIT_TIMEOUT) {
		return "", fmt.Errorf("parent shell exited")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", created.HighDateTime, created.LowDateTime), nil
}
