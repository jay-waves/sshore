package files

import (
	"context"
	"os"
	"strings"
	"syscall"
	"time"
)

const metadataRefreshLimit = 64
const metadataRefreshRecent = 16
const metadataRefreshDebounce = 100 * time.Millisecond
const metadataRefreshMaxWait = 500 * time.Millisecond
const metadataRefreshInterval = 100 * time.Millisecond

type metadataRefreshTask struct {
	priority    uint64
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
	cancel      context.CancelFunc
	wake        chan struct{}
	done        chan struct{}
	queue       map[string]metadataRefreshTask
	next        uint64
	nextAllowed time.Time
	failures    int
}

func startMetadataRefresh(parent context.Context, fs *ftpFS) *metadataRefresh {
	ctx, cancel := context.WithCancel(parent)
	r := &metadataRefresh{fs: fs, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), queue: make(map[string]metadataRefreshTask)}
	fs.cache.mu.Lock()
	fs.cache.refresh = r
	fs.cache.mu.Unlock()
	go r.run()
	return r
}

func (r *metadataRefresh) scheduleLocked(key string) {
	if r == nil || r.ctx.Err() != nil {
		return
	}
	e := r.fs.cache.entries[key]
	if e == nil {
		return
	}
	v := e.Value.(ftpCacheValue)
	if !time.Now().Before(v.expires) || (v.fetch == nil && !strings.HasPrefix(key, "s:")) {
		return
	}
	now := time.Now()
	task, exists := r.queue[key]
	if !exists {
		task.first = now
		r.fs.cache.counters.RefreshQueued++
	} else {
		r.fs.cache.counters.RefreshCoalesced++
	}
	r.next++
	task.last, task.priority = now, r.next
	r.queue[key] = task
	if len(r.queue) > metadataRefreshLimit {
		oldest := key
		for k, task := range r.queue {
			if task.priority < r.queue[oldest].priority {
				oldest = k
			}
		}
		delete(r.queue, oldest)
		r.fs.cache.counters.RefreshDropped++
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *metadataRefresh) scheduleRecentLocked() {
	if r == nil {
		return
	}
	var keys []string
	for e := r.fs.cache.lru.Front(); e != nil && len(keys) < metadataRefreshRecent; e = e.Next() {
		keys = append(keys, e.Value.(ftpCacheValue).key)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		r.scheduleLocked(keys[i])
	}
}

// A negative wait means there is no runnable work. Expired and superseded
// entries are retired before they can consume request capacity.
func (r *metadataRefresh) nextLocked(now time.Time) (string, time.Duration) {
	c := &r.fs.cache
	if c.writers != 0 || len(c.mutations) != 0 {
		return "", -1
	}
	var key string
	var priority uint64
	var earliest time.Time
	for k, task := range r.queue {
		e := c.entries[k]
		if e == nil || e.Value.(ftpCacheValue).revision == c.revision || !now.Before(e.Value.(ftpCacheValue).expires) {
			delete(r.queue, k)
			continue
		}
		ready := task.readyAt()
		if earliest.IsZero() || ready.Before(earliest) {
			earliest = ready
		}
		if !ready.After(now) && task.priority > priority {
			key, priority = k, task.priority
		}
	}
	if earliest.IsZero() {
		return "", -1
	}
	if r.nextAllowed.After(earliest) {
		earliest = r.nextAllowed
	}
	if earliest.After(now) {
		return "", earliest.Sub(now)
	}
	return key, 0
}

func (r *metadataRefresh) run() {
	defer close(r.done)
	for r.ctx.Err() == nil {
		r.fs.cache.mu.Lock()
		_, delay := r.nextLocked(time.Now())
		r.fs.cache.mu.Unlock()
		if delay == 0 {
			r.step()
			continue
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if delay > 0 {
			timer = time.NewTimer(delay)
			tick = timer.C
		}
		select {
		case <-r.ctx.Done():
		case <-r.wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (r *metadataRefresh) step() bool {
	c := &r.fs.cache
	c.mu.Lock()
	key, wait := r.nextLocked(time.Now())
	if r.ctx.Err() != nil || wait != 0 || key == "" {
		c.mu.Unlock()
		return false
	}
	delete(r.queue, key)
	e := c.entries[key]
	if e == nil {
		c.mu.Unlock()
		return true
	}
	old := e.Value.(ftpCacheValue)
	if old.revision == c.revision || !time.Now().Before(old.expires) {
		c.mu.Unlock()
		return true
	}
	token, revision := ftpCacheToken{c.epoch, time.Now()}, c.revision
	r.nextAllowed = token.started.Add(metadataRefreshInterval)
	c.mu.Unlock()

	fresh, err := r.fetch(old)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) && r.ctx.Err() == nil {
		r.failures = min(r.failures+1, 5)
		r.nextAllowed = time.Now().Add(min(500*time.Millisecond*time.Duration(1<<(r.failures-1)), 5*time.Second))
	} else if err == nil {
		r.failures = 0
	}
	// A later mutation, notification, eviction or foreground fetch wins.
	if r.ctx.Err() != nil || c.entries[key] != e || c.revision != revision || c.epoch != token.epoch {
		c.counters.RefreshDiscarded++
		return true
	}
	if err != nil {
		c.removeLocked(e)
		c.counters.RefreshErrors++
		return true
	}
	// Verification must not make an old entry look like a new user access.
	var previous string
	if before := e.Prev(); before != nil {
		previous = before.Value.(ftpCacheValue).key
	}
	fresh.key, fresh.scope, fresh.fetch = key, old.scope, old.fetch
	fresh.revision, fresh.prefetched = revision, old.prefetched
	ttl := ftpMetadataTTL
	if fresh.directory {
		ttl = ftpDirectoryTTL
		c.directoryLocked(token, strings.TrimPrefix(key, "d:"), fresh.entries)
	}
	fresh.expires = token.started.Add(ttl)
	c.putLocked(token, fresh)
	if current, before := c.entries[key], c.entries[previous]; current != nil && before != nil {
		c.lru.MoveAfter(current, before)
	}
	c.counters.RefreshCompleted++
	return true
}

func (r *metadataRefresh) fetch(v ftpCacheValue) (ftpCacheValue, error) {
	if strings.HasPrefix(v.key, "d:") {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		defer cancel()
		entries, err := r.fs.client.ReadDirContext(ctx, v.key[2:])
		if len(entries) > ftpMaxDirectoryEntries {
			return ftpCacheValue{}, syscall.EOVERFLOW
		}
		size := 256 + len(v.key[2:])
		for _, info := range entries {
			size += ftpInfoBytes(info)
		}
		return ftpCacheValue{entries: entries, directory: true, bytes: size}, err
	}
	if strings.HasPrefix(v.key, "s:") {
		info, err := r.fs.client.Stat(v.key[2:])
		return ftpCacheValue{info: info, bytes: 256 + len(v.key[2:]) + ftpInfoBytes(info)}, err
	}
	fresh, err := v.fetch()
	fresh.bytes += len(v.scope)
	return fresh, err
}
