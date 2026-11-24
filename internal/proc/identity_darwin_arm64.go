package proc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func liveProcess(pid int) (*unix.ExternProc, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	const zombie = 5 // Darwin SZOMB
	p := &info.Proc
	if pid <= 0 || int(p.P_pid) != pid || info.Eproc.Ucred.Uid != uint32(os.Geteuid()) || p.P_stat == 0 || p.P_stat == zombie {
		return nil, fmt.Errorf("process %d is unavailable", pid)
	}
	return p, nil
}
func LiveIdentity(pid int) (string, error) {
	p, e := liveProcess(pid)
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("%d:%06d", p.P_starttime.Sec, p.P_starttime.Usec), nil
}
