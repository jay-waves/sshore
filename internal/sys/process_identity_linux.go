package sys

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func LiveIdentity(pid int) (string, error) {
	path := filepath.Join(procRoot, strconv.Itoa(pid))
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("process %d is not owned by this user", pid)
	}
	data, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return "", err
	}
	// comm can contain spaces, newlines, and parentheses.
	stat := string(data)
	start, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if start < 0 || end <= start {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("short process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", fmt.Errorf("parent shell exited")
	}
	return fields[19], nil
}
