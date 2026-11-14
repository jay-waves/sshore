package main

// Mount inventory uses the existing SFTP transport. Failure to publish metadata
// never fails file IO. Shutdown cleanup is bounded; a stalled server leaves an
// expiring advisory record rather than holding unmount hostage.
import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/sftp"
	"os"
	"path"
	"time"
)

func publishSFTPTask(c *sftp.Client, dir string, t remoteTask) error {
	if e := validateRemoteTask(t); e != nil {
		return e
	}
	if _, e := c.Lstat(dir); os.IsNotExist(e) {
		if e = c.MkdirAll(dir); e != nil {
			return e
		}
		if e = c.Chmod(dir, 0700); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	info, e := c.Lstat(dir)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("peer task directory is not private")
	}
	data, e := json.Marshal(t)
	if e != nil {
		return e
	}
	temp := path.Join(dir, ".task-"+t.Key)
	f, e := c.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return e
	}
	defer c.Remove(temp)
	if e = c.Chmod(temp, 0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	final := path.Join(dir, t.Key+".json")
	if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
		return c.PosixRename(temp, final)
	}
	_ = c.Remove(final)
	return c.Rename(temp, final)
}
func startMountTaskSync(parent context.Context, c *sftp.Client, t remoteTask) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		home, e := c.RealPath(".")
		if e != nil {
			return
		}
		dir := path.Join(home, remoteTaskRelativeDir)
		defer c.Remove(path.Join(dir, t.Key+".json"))
		ticker := time.NewTicker(remoteTaskRefresh)
		defer ticker.Stop()
		for {
			_ = publishSFTPTask(c, dir, t)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
}
