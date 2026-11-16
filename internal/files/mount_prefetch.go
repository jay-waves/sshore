package files

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

const directoryPrefetchLimit = 16
const directoryPrefetchWorkers = 4

// Workers share a replaceable queue. No speculative read schedules descendants.
type directoryPrefetch struct {
	fs          *ftpFS
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	paths       []string
	batch       context.Context
	cancelBatch context.CancelFunc
	lastParent  string
	lastPaths   []string
	scheduled   time.Time
	wake        chan struct{}
	done        chan struct{}
}

func startDirectoryPrefetch(parent context.Context, fs *ftpFS) *directoryPrefetch {
	ctx, cancel := context.WithCancel(parent)
	p := &directoryPrefetch{fs: fs, ctx: ctx, cancel: cancel, wake: make(chan struct{}, directoryPrefetchWorkers), done: make(chan struct{})}
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < directoryPrefetchWorkers; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); p.run() }()
		}
		wg.Wait()
		close(p.done)
	}()
	return p
}
func (p *directoryPrefetch) schedule(parent string, entries []os.FileInfo) {
	if p == nil || p.ctx.Err() != nil {
		return
	}
	paths := make([]string, 0, directoryPrefetchLimit+1)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".") || ValidateName(entry.Name()) != nil {
			continue
		}
		candidate := path.Join(parent, entry.Name())
		at, _ := slices.BinarySearch(paths, candidate)
		if at >= directoryPrefetchLimit {
			continue
		}
		paths = slices.Insert(paths, at, candidate)
		if len(paths) > directoryPrefetchLimit {
			paths = paths[:directoryPrefetchLimit]
		}
	}
	p.mu.Lock()
	if parent == p.lastParent && slices.Equal(paths, p.lastPaths) && time.Since(p.scheduled) < ftpDirectoryTTL {
		p.mu.Unlock()
		return
	}
	p.lastParent, p.lastPaths, p.scheduled = parent, paths, time.Now()
	if p.cancelBatch != nil {
		p.cancelBatch()
	}
	p.batch, p.cancelBatch = context.WithCancel(p.ctx)
	p.paths = paths
	p.mu.Unlock()
	for i := 0; i < directoryPrefetchWorkers; i++ {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}
func (p *directoryPrefetch) run() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		}
		for p.ctx.Err() == nil {
			p.mu.Lock()
			if len(p.paths) == 0 {
				p.mu.Unlock()
				break
			}
			dir, batch := p.paths[0], p.batch
			p.paths = p.paths[1:]
			p.mu.Unlock()
			if batch.Err() != nil {
				continue
			}
			// Registration affects only this directory's cache; another worker's
			// already-prefetched sibling remains usable.
			p.fs.notifications.ensure(dir)
			if batch.Err() != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(batch, 2*time.Second)
			_, _ = p.fs.loadDirectoryMode(ctx, dir, true)
			cancel()
		}
	}
}
