package files

import (
	"container/list"
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const metadataRefreshLimit = 64
const metadataRefreshRecent = 16
const metadataRefreshDebounce = 100 * time.Millisecond
const metadataRefreshMaxWait = 500 * time.Millisecond
const metadataRefreshInterval = 100 * time.Millisecond

type metadataRefreshTask struct {
	entry       *list.Element
	first, last time.Time
}

func (t metadataRefreshTask) readyAt() time.Time {
	quiet, deadline := t.last.Add(metadataRefreshDebounce), t.first.Add(metadataRefreshMaxWait)
	if quiet.Before(deadline) {
		return quiet
	}
	return deadline
}

// One background request competes with neither foreground cache flights nor
// their waiters. Queue ownership is under cache.mu; newer accesses take priority.
type metadataRefresh struct {
	fs          *ftpFS
	ctx         context.Context
	fetch       func(context.Context, ftpCacheKey) (ftpCacheValue, error)
	wake        chan struct{}
	done        chan struct{}
	queue       []metadataRefreshTask // Oldest access first.
	nextAllowed time.Time
	failures    int
}

func startMetadataRefresh(ctx context.Context, fs *ftpFS) *metadataRefresh {
	r := &metadataRefresh{fs: fs, ctx: ctx, fetch: fs.fetchMetadata, wake: make(chan struct{}, 1), done: make(chan struct{})}
	fs.cache.mu.Lock()
	fs.cache.refresh = r
	fs.cache.mu.Unlock()
	go r.run()
	return r
}

func (r *metadataRefresh) enqueueLocked(e *list.Element) bool {
	if r == nil || r.ctx.Err() != nil {
		return false
	}
	c := &r.fs.cache
	v := e.Value.(ftpCacheValue)
	if v.prefetched || v.revision == c.revision {
		return false
	}
	now := time.Now()
	if !now.Before(v.expires) || r.fetch == nil {
		return false
	}
	task := metadataRefreshTask{entry: e, first: now, last: now}
	coalesced := false
	r.queue = slices.DeleteFunc(r.queue, func(pending metadataRefreshTask) bool {
		if pending.entry == e {
			task.first = pending.first
			coalesced = true
			return true
		}
		return pending.entry.Value == nil
	})
	if coalesced {
		c.counters.RefreshCoalesced++
	} else {
		c.counters.RefreshQueued++
		if len(r.queue) == metadataRefreshLimit {
			r.queue = slices.Delete(r.queue, 0, 1)
			c.counters.RefreshDropped++
		}
	}
	r.queue = append(r.queue, task)
	return true
}

func (r *metadataRefresh) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *metadataRefresh) scheduleRecentLocked() {
	if r == nil {
		return
	}
	c := &r.fs.cache
	var last *list.Element
	for e, count := c.lru.Front(), 0; e != nil && count < metadataRefreshRecent; e, count = e.Next(), count+1 {
		last = e
	}
	for e := last; e != nil; e = e.Prev() {
		r.enqueueLocked(e)
	}
	// Completing the last mutation can unblock work already in the queue.
	if len(r.queue) != 0 {
		r.signal()
	}
}

// A negative wait means there is no runnable work. Detached and expired nodes
// can be discarded in the same pass that picks the next refresh.
func (r *metadataRefresh) nextLocked(now time.Time) (int, time.Duration) {
	c := &r.fs.cache
	if c.writers != 0 || len(c.mutations) != 0 {
		return -1, -1
	}
	chosen := -1
	var earliest time.Time
	live := r.queue[:0]
	for _, task := range r.queue {
		v, liveEntry := task.entry.Value.(ftpCacheValue)
		if !liveEntry || !now.Before(v.expires) {
			continue
		}
		ready := task.readyAt()
		if earliest.IsZero() || ready.Before(earliest) {
			earliest = ready
		}
		if !ready.After(now) {
			chosen = len(live)
		}
		live = append(live, task)
	}
	clear(r.queue[len(live):])
	r.queue = live
	if earliest.IsZero() {
		return -1, -1
	}
	if r.nextAllowed.After(earliest) {
		earliest = r.nextAllowed
	}
	if earliest.After(now) {
		return -1, earliest.Sub(now)
	}
	return chosen, 0
}

func (r *metadataRefresh) run() {
	defer close(r.done)
	timer := time.NewTimer(metadataRefreshInterval)
	timer.Stop()
	defer timer.Stop()
	for r.ctx.Err() == nil {
		delay := r.step()
		if delay == 0 {
			continue
		}
		var tick <-chan time.Time
		if delay > 0 {
			timer.Reset(delay)
			tick = timer.C
		}
		select {
		case <-r.ctx.Done():
		case <-r.wake:
		case <-tick:
		}
		timer.Stop()
	}
}

// Run one ready verification or return the delay before the next one.
func (r *metadataRefresh) step() time.Duration {
	c := &r.fs.cache
	c.mu.Lock()
	index, wait := r.nextLocked(time.Now())
	if r.ctx.Err() != nil || wait != 0 {
		c.mu.Unlock()
		return wait
	}
	e := r.queue[index].entry
	r.queue = slices.Delete(r.queue, index, index+1)
	old := e.Value.(ftpCacheValue)
	key := old.key
	started, revision := time.Now(), c.revision
	r.nextAllowed = started.Add(metadataRefreshInterval)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
	fresh, err := key.read(ctx, r.fetch, started, revision)
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) && r.ctx.Err() == nil {
		r.failures = min(r.failures+1, 5)
		r.nextAllowed = time.Now().Add(min(500*time.Millisecond*time.Duration(1<<(r.failures-1)), 5*time.Second))
	} else if err == nil {
		r.failures = 0
	}
	// A later mutation, notification, eviction or foreground fetch wins.
	if r.ctx.Err() != nil || c.entries[key] != e || c.revision != revision {
		c.counters.RefreshDiscarded++
		return 0
	}
	if err != nil {
		c.removeLocked(e)
		c.counters.RefreshErrors++
		return 0
	}
	if c.storeLocked(fresh, true) {
		c.counters.RefreshCompleted++
	} else {
		c.counters.RefreshDiscarded++
	}
	return 0
}

const directoryPrefetchCount = 10
const directoryPrefetchConcurrency = 4

type directoryPrefetchBatch struct {
	targets [directoryPrefetchCount]string
	ctx     context.Context
	cancel  context.CancelFunc
}

type directoryPrefetch struct {
	fs             *ftpFS
	ctx            context.Context
	mu             sync.Mutex
	history, delta VisitHistory
	lastParent     string
	scheduled      time.Time
	latest         *directoryPrefetchBatch
	pending        chan *directoryPrefetchBatch
	done           chan struct{}
}

func startDirectoryPrefetch(ctx context.Context, fs *ftpFS, history VisitHistory) *directoryPrefetch {
	p := &directoryPrefetch{fs: fs, ctx: ctx, history: history, delta: VisitHistory{}, pending: make(chan *directoryPrefetchBatch, 1), done: make(chan struct{})}
	go p.run()
	return p
}
func (p *directoryPrefetch) schedule(parent string, entries []os.FileInfo) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return
	}
	now := time.Now()
	prefix := strings.TrimSuffix(p.fs.root, "/") + "/"
	if parent != p.fs.root && !strings.HasPrefix(parent, prefix) {
		return
	}
	if parent != p.lastParent && parent != p.fs.root {
		key := strings.TrimPrefix(parent, prefix)
		p.delta.add(key, 1, now)
	}
	type candidate struct {
		dir    string
		weight float64
	}
	var ranked [directoryPrefetchCount]candidate
	visible := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !e.IsDir() || e.Mode()&os.ModeSymlink != 0 || ValidateName(name) != nil {
			continue
		}
		visible++
		dir := path.Join(parent, name)
		key := strings.TrimPrefix(dir, prefix)
		weight := p.history[key].weight(now) + p.delta[key].weight(now)
		if visible > directoryPrefetchCount && weight == 0 {
			continue
		}
		for i, other := range ranked[:] {
			if dir == other.dir {
				break
			}
			if other.dir == "" || weight > other.weight || weight == other.weight && dir < other.dir {
				copy(ranked[i+1:], ranked[i:])
				ranked[i] = candidate{dir: dir, weight: weight}
				break
			}
		}
	}
	var best [directoryPrefetchCount]string
	for i, entry := range ranked {
		if visible > directoryPrefetchCount && entry.weight == 0 {
			break
		}
		best[i] = entry.dir
	}
	var previous [directoryPrefetchCount]string
	if p.latest != nil {
		previous = p.latest.targets
	}
	if parent == p.lastParent && best == previous && now.Sub(p.scheduled) < ftpDirectoryTTL {
		return
	}
	p.lastParent, p.scheduled = parent, now
	if p.latest != nil {
		p.latest.cancel()
		p.latest = nil
	}
	// Scheduling owns replacement of the single pending batch. The worker
	// can take the immutable batch directly without reacquiring the lock.
	select {
	case <-p.pending:
	default:
	}
	if best[0] != "" {
		ctx, cancel := context.WithCancel(p.ctx)
		p.latest = &directoryPrefetchBatch{targets: best, ctx: ctx, cancel: cancel}
		p.pending <- p.latest
	}
}
func (p *directoryPrefetch) run() {
	defer close(p.done)
	for {
		select {
		case <-p.ctx.Done():
			return
		case batch := <-p.pending:
			if batch.ctx.Err() == nil {
				p.prefetchBatch(batch.ctx, batch.targets[:])
			}
			batch.cancel()
		}
	}
}

func (p *directoryPrefetch) prefetchBatch(ctx context.Context, dirs []string) {
	if end := slices.Index(dirs, ""); end >= 0 {
		dirs = dirs[:end]
	}
	var next atomic.Int32
	var workers sync.WaitGroup
	for range min(len(dirs), directoryPrefetchConcurrency) {
		workers.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(dirs) {
					return
				}
				dir := dirs[i]
				p.fs.notifications.ensure(ctx, dir)
				if ctx.Err() != nil {
					return
				}
				listing, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, _ = p.fs.loadDirectoryMode(listing, dir, true)
				cancel()
			}
		})
	}
	workers.Wait()
}
