package sys

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// delegatedCgroup locates the daemon's own writable cgroup v2 directory.
func delegatedCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		path, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "0::")
		if !ok || !filepath.IsAbs(path) {
			continue
		}
		rel := strings.TrimPrefix(path, "/")
		if rel != "" && !filepath.IsLocal(rel) {
			return "", errors.New("daemon cgroup is outside the cgroup namespace")
		}
		dir := filepath.Join("/sys/fs/cgroup", path)
		var stat unix.Statfs_t
		if err := unix.Statfs(dir, &stat); err != nil {
			return "", err
		}
		if stat.Type != unix.CGROUP2_SUPER_MAGIC {
			return "", errors.New("/sys/fs/cgroup is not a cgroup v2 mount")
		}
		if err := unix.Access(filepath.Join(dir, "cgroup.procs"), unix.W_OK); err != nil {
			return "", err
		}
		if err := unix.Access(dir, unix.W_OK); err != nil {
			return "", err
		}
		return dir, nil
	}
	return "", errors.New("cgroup v2 is unavailable")
}

// TaskCgroup owns a private child of the daemon's delegated cgroup. The kill
// descriptor stays bound to that child even if a directory name is reused.
type TaskCgroup struct {
	dir    string
	kill   *os.File
	mu     sync.Mutex
	killed bool
}

func NewTaskCgroup(parent string) (*TaskCgroup, *os.File, error) {
	if parent == "" {
		return nil, nil, errors.New("a delegated cgroup v2 subtree is required")
	}
	path, err := os.MkdirTemp(parent, "sshore-task-")
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.Open(path)
	if err == nil {
		fd, openErr := unix.Openat(int(dir.Fd()), "cgroup.kill", unix.O_WRONLY|unix.O_CLOEXEC, 0)
		if openErr == nil {
			return &TaskCgroup{dir: path, kill: os.NewFile(uintptr(fd), "cgroup.kill")}, dir, nil
		}
		err = openErr
		_ = dir.Close()
	}
	return nil, nil, errors.Join(err, os.Remove(path))
}

func (g *TaskCgroup) Kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.killed {
		return nil
	}
	if _, err := g.kill.WriteString("1"); err != nil {
		return fmt.Errorf("stop task cgroup: %w", err)
	}
	g.killed = true
	return nil
}

// Remove is called by the child reaper, or by the launcher if exec fails.
func (g *TaskCgroup) Remove() error {
	defer g.kill.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := removeCgroupTree(g.dir)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) || !time.Now().Before(deadline) {
			return fmt.Errorf("remove task cgroup: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func removeCgroupTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeCgroupTree(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return unix.Rmdir(dir)
}
