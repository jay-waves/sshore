package files

import (
	"container/list"
	"context"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
)

const ftpMetadataLimit = 4096
const ftpMetadataTTL = time.Second
const ftpCapacityTTL = 2 * time.Second
const ftpDirectoryTTL = 5 * time.Second
const ftpDirectoryCacheLimit = 128
const ftpCacheBytes = 32 * 1024 * 1024
const ftpCacheDirectoryEntries = 32768

type ftpCacheKind uint8

const (
	ftpStatKey ftpCacheKind = iota + 1
	ftpLstatKey
	ftpDirectoryKey
	ftpCapacityKey
	ftpHandleKey
)

type ftpCacheKey struct {
	kind ftpCacheKind
	name string // Remote path, or containing directory for a handle.
	file *ftpFile
}

func (k ftpCacheKey) parentDirectory() string {
	switch k.kind {
	case ftpStatKey, ftpLstatKey, ftpDirectoryKey:
		return path.Dir(k.name)
	case ftpHandleKey:
		return k.name
	default:
		return ""
	}
}

func (k ftpCacheKey) lifetime() time.Duration {
	switch k.kind {
	case ftpDirectoryKey:
		return ftpDirectoryTTL
	case ftpCapacityKey:
		return ftpCapacityTTL
	default:
		return ftpMetadataTTL
	}
}

type ftpCacheValue struct {
	key         ftpCacheKey
	info        os.FileInfo
	capacity    *sftp.StatVFS
	capacityErr error
	entries     []os.FileInfo
	expires     time.Time
	bytes       int
	prefetched  bool
	revision    uint64
}

func (v ftpCacheValue) isDirectory() bool { return v.key.kind == ftpDirectoryKey }

// Cache entries retain enough information in the key to repeat the query.
func (k ftpCacheKey) read(ctx context.Context, fetch func(context.Context, ftpCacheKey) (ftpCacheValue, error), started time.Time, revision uint64) (ftpCacheValue, error) {
	result, err := fetch(ctx, k)
	result.key, result.expires = k, started.Add(k.lifetime())
	result.revision = revision
	if k.kind == ftpHandleKey {
		result.bytes += len(k.name)
	}
	return result, err
}

type ftpCacheFlight struct {
	done        chan struct{}
	speculative bool
	value       ftpCacheValue
	err         error
}
type ftpCacheCounters struct {
	Hits, Misses, Shared, Evictions, RemoteInvalidations, SubscriptionConfirmations uint64
	PrefetchStored, PrefetchHits, PrefetchDiscarded                                 uint64
	RefreshQueued, RefreshCoalesced, RefreshDropped                                 uint64
	RefreshCompleted, RefreshErrors, RefreshDiscarded                               uint64
}

// Cached slices are immutable. Each directory handle retains its own cursor.
type ftpMetadataCache struct {
	mu        sync.Mutex
	entries   map[ftpCacheKey]*list.Element
	lru       list.List // Foreground entries, newest first.
	prefetch  list.List // Unused prefetches, newest first.
	flights   map[ftpCacheKey]*ftpCacheFlight
	writers   int
	mutations map[string]int
	revision  uint64
	// Changes on mutations and invalidation, independently of refresh revisions.
	handleEpoch             atomic.Uint64
	refresh                 *metadataRefresh
	bytes, dirs, dirEntries int
	counters                ftpCacheCounters
}

func (c *ftpMetadataCache) metadataEntriesLocked() int { return len(c.entries) - c.dirs }

func (c *ftpMetadataCache) resetLocked() {
	c.handleEpoch.Add(1)
	c.counters.PrefetchDiscarded += uint64(c.prefetch.Len())
	c.entries = nil
	c.lru.Init()
	c.prefetch.Init()
	c.bytes, c.dirs, c.dirEntries = 0, 0, 0
	// Existing callers still own their flights; new callers must not join them.
	c.flights = nil
	if c.refresh != nil {
		c.refresh.queue = nil
	}
}
func (c *ftpMetadataCache) removeLocked(e *list.Element) {
	v := e.Value.(ftpCacheValue)
	if v.prefetched {
		c.prefetch.Remove(e)
		c.counters.PrefetchDiscarded++
	} else {
		c.lru.Remove(e)
	}
	// Queued refreshes can still reference the detached node. Drop its payload
	// immediately so they cannot keep an evicted listing or handle alive.
	e.Value = nil
	delete(c.entries, v.key)
	c.bytes -= v.bytes
	if v.isDirectory() {
		c.dirs--
		c.dirEntries -= len(v.entries)
	}
}
func (c *ftpMetadataCache) admitsLocked(v ftpCacheValue) bool {
	if c.mutatingLocked(v.key) || !time.Now().Before(v.expires) || v.bytes > ftpCacheBytes || len(v.entries) > ftpCacheDirectoryEntries {
		return false
	}
	old := c.entries[v.key]
	return old == nil || !old.Value.(ftpCacheValue).expires.After(v.expires)
}

func (c *ftpMetadataCache) storeLocked(v ftpCacheValue, preserveRecency bool) bool {
	if !c.admitsLocked(v) {
		return false
	}
	// Foreground listings enter ahead of their child attributes. Refreshes
	// replace the listing in place, so they cannot promote a cold directory.
	if !preserveRecency && c.seedDirectoryLocked(v) && !time.Now().Before(v.expires) {
		return false
	}
	e := c.insertLocked(v, preserveRecency)
	if preserveRecency && c.entries[v.key] == e {
		c.seedDirectoryLocked(v)
	}
	return c.entries[v.key] == e
}

// Insert an admitted entry and enforce the shared cache budget.
func (c *ftpMetadataCache) insertLocked(v ftpCacheValue, preserveRecency bool) *list.Element {
	recency := &c.lru
	if v.prefetched {
		recency = &c.prefetch
		c.counters.PrefetchStored++
	}
	var e *list.Element
	if old := c.entries[v.key]; old != nil {
		if preserveRecency && v.prefetched == old.Value.(ftpCacheValue).prefetched {
			e = recency.InsertBefore(v, old)
		}
		c.removeLocked(old)
	}
	if c.entries == nil {
		c.entries = make(map[ftpCacheKey]*list.Element)
	}
	if e == nil {
		e = recency.PushFront(v)
	}
	c.entries[v.key] = e
	c.bytes += v.bytes
	if v.isDirectory() {
		c.dirs++
		c.dirEntries += len(v.entries)
	}
	for c.bytes > ftpCacheBytes || c.metadataEntriesLocked() > ftpMetadataLimit || c.dirs > ftpDirectoryCacheLimit || c.dirEntries > ftpCacheDirectoryEntries {
		oldest := c.prefetch.Back()
		if oldest == nil {
			oldest = c.lru.Back()
		}
		c.removeLocked(oldest)
		c.counters.Evictions++
	}
	return e
}
func (c *ftpMetadataCache) loadMode(ctx context.Context, key ftpCacheKey, speculative bool, fetch func(context.Context, ftpCacheKey) (ftpCacheValue, error)) (ftpCacheValue, error) {
	c.mu.Lock()
	started, revision := time.Now(), c.revision
	if c.mutatingLocked(key) {
		// Readers racing a mutation do their own fetch without touching recency,
		// joining a cached lookup, or publishing their result.
		c.counters.Misses++
		c.mu.Unlock()
		return key.read(ctx, fetch, started, revision)
	}
	if !speculative {
		parent := key.parentDirectory()
		if dir := c.entries[ftpCacheKey{kind: ftpDirectoryKey, name: parent}]; parent != "" && dir != nil {
			v := dir.Value.(ftpCacheValue)
			if !v.prefetched && started.Before(v.expires) {
				c.lru.MoveToFront(dir)
			}
		}
	}
	if e := c.entries[key]; e != nil {
		v := e.Value.(ftpCacheValue)
		if started.Before(v.expires) {
			if !speculative {
				c.useLocked(e)
			}
			c.counters.Hits++
			c.mu.Unlock()
			return v, nil
		}
		c.removeLocked(e)
	}
	if f := c.flights[key]; f != nil {
		if !speculative {
			f.speculative = false
		}
		c.counters.Shared++
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.value, f.err
		case <-ctx.Done():
			return ftpCacheValue{}, ctx.Err()
		}
	}
	c.counters.Misses++
	flight := &ftpCacheFlight{done: make(chan struct{}), speculative: speculative}
	if c.flights == nil {
		c.flights = make(map[ftpCacheKey]*ftpCacheFlight)
	}
	c.flights[key] = flight
	c.mu.Unlock()
	v, err := key.read(ctx, fetch, started, revision)
	c.mu.Lock()
	// Invalidation detaches the flight while its existing readers retain it.
	// Only the current flight may retire its slot and publish to the cache.
	v.prefetched = flight.speculative
	if c.flights[key] == flight {
		delete(c.flights, key)
		if err == nil {
			c.storeLocked(v, false)
			if speculative && !v.prefetched {
				c.counters.PrefetchHits++
			}
		}
	}
	flight.value, flight.err = v, err
	close(flight.done)
	c.mu.Unlock()
	return v, err
}

func (c *ftpMetadataCache) useLocked(e *list.Element) {
	v := e.Value.(ftpCacheValue)
	if !v.prefetched {
		c.lru.MoveToFront(e)
		if c.refresh.enqueueLocked(e) {
			c.refresh.signal()
		}
		return
	}
	c.prefetch.Remove(e)
	v.prefetched = false
	c.entries[v.key] = c.lru.PushFront(v)
	c.counters.PrefetchHits++
	c.seedDirectoryLocked(v)
}

func statCacheValue(name string, info os.FileInfo) ftpCacheValue {
	return ftpCacheValue{info: info, bytes: 256 + len(name) + ftpInfoBytes(info)}
}

func directoryValue(name string, entries []os.FileInfo) ftpCacheValue {
	size := 256 + len(name)
	for _, info := range entries {
		size += ftpInfoBytes(info)
	}
	return ftpCacheValue{entries: entries, bytes: size}
}
func (c *ftpMetadataCache) seedDirectoryLocked(listing ftpCacheValue) bool {
	if !listing.isDirectory() || listing.prefetched || listing.revision != c.revision {
		return false
	}
	expires := listing.expires.Add(ftpMetadataTTL - ftpDirectoryTTL)
	if !time.Now().Before(expires) {
		return false
	}
	name := listing.key.name
	for _, info := range listing.entries[:min(len(listing.entries), ftpMetadataLimit)] {
		if info.Mode()&os.ModeSymlink != 0 || ValidateName(info.Name()) != nil {
			continue
		}
		child := path.Join(name, info.Name())
		v := statCacheValue(child, info)
		v.key, v.revision = ftpCacheKey{kind: ftpStatKey, name: child}, c.revision
		v.expires = expires
		if c.admitsLocked(v) {
			c.insertLocked(v, false)
		}
	}
	return true
}

// Paths include both sides of a rename. Unknown scopes retain the full reset.
func (c *ftpMetadataCache) beginMutation(paths ...string) func() {
	c.mu.Lock()
	c.changeMutationLocked(paths, 1)
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.changeMutationLocked(paths, -1)
		c.refresh.scheduleRecentLocked()
	}
}

func (c *ftpMetadataCache) changeMutationLocked(paths []string, delta int) {
	c.revision++
	if len(paths) == 0 {
		c.writers += delta
		c.resetLocked()
		return
	}
	if c.mutations == nil {
		c.mutations = make(map[string]int)
	}
	for _, name := range paths {
		c.mutations[name] += delta
		if c.mutations[name] == 0 {
			delete(c.mutations, name)
		}
	}
	c.invalidateLocked(func(key ftpCacheKey) bool {
		for _, name := range paths {
			if mutationAffects(key, name) {
				return true
			}
		}
		return false
	})
}

func mutationAffects(key ftpCacheKey, name string) bool {
	if key.kind == ftpCapacityKey {
		return true
	}
	parent := path.Dir(name)
	if key.kind == ftpHandleKey && key.name == "" {
		return true
	}
	return key.name == name || key.name == parent || strings.HasPrefix(key.name, strings.TrimSuffix(name, "/")+"/")
}

func (c *ftpMetadataCache) mutatingLocked(key ftpCacheKey) bool {
	if c.writers != 0 {
		return true
	}
	for name := range c.mutations {
		if mutationAffects(key, name) {
			return true
		}
	}
	return false
}

// Retire values, pending refreshes and flights together under the cache lock.
func (c *ftpMetadataCache) invalidateLocked(affected func(key ftpCacheKey) bool) {
	c.handleEpoch.Add(1)
	for key, e := range c.entries {
		if affected(key) {
			c.removeLocked(e)
		}
	}
	for key := range c.flights {
		if affected(key) {
			delete(c.flights, key)
		}
	}
}

func ftpInfoBytes(info os.FileInfo) int {
	if info == nil {
		return 0
	}
	size := 256 + len(info.Name())
	if ext, ok := info.(sftp.FileInfoExtendedData); ok {
		for _, v := range ext.Extended() {
			size += 32 + len(v.ExtType) + len(v.ExtData)
		}
	}
	return size
}

func (c *ftpMetadataCache) invalidateRemote() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters.RemoteInvalidations++
	c.resetLocked()
}

// Registration closes an unobserved interval for one directory, not the entire
// mount. Retire matching in-flight fetches as well as existing entries.
func (c *ftpMetadataCache) registered(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateDirectoryLocked(dir)
	c.counters.SubscriptionConfirmations++
}
func (c *ftpMetadataCache) invalidateDirectory(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateDirectoryLocked(dir)
	c.counters.RemoteInvalidations++
}
func (c *ftpMetadataCache) invalidateDirectoryLocked(dir string) {
	c.invalidateLocked(func(key ftpCacheKey) bool {
		switch key.kind {
		case ftpCapacityKey:
			return true
		case ftpHandleKey:
			return key.name == "" || key.name == dir
		default:
			return key.name == dir || key.kind != ftpDirectoryKey && path.Dir(key.name) == dir
		}
	})
}
