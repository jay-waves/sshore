package mount

import (
	"sort"
	"sync"
)

// Coalesce by directory while the network writer is busy. Lost detail always
// becomes a full reset rather than an incomplete invalidation list.
type watchChanges struct {
	mu    sync.Mutex
	dirs  map[string]struct{}
	reset bool
}

func (c *watchChanges) add(dir string, reset bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reset {
		return
	}
	if reset || !validFTPWatchPath(dir) {
		c.reset = true
		c.dirs = nil
		return
	}
	if c.dirs == nil {
		c.dirs = make(map[string]struct{})
	}
	c.dirs[dir] = struct{}{}
	if len(c.dirs) > ftpWatchLimit {
		c.reset = true
		c.dirs = nil
	}
}
func (c *watchChanges) take() (bool, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reset := c.reset
	dirs := make([]string, 0, len(c.dirs))
	for dir := range c.dirs {
		dirs = append(dirs, dir)
	}
	c.reset = false
	c.dirs = nil
	sort.Strings(dirs)
	return reset, dirs
}
