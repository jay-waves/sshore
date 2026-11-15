package main

import (
	"container/list"
	"os"
	"path"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

const ftpMetadataLimit = 4096
const ftpMetadataTTL = time.Second
const ftpDirectoryTTL = 2 * time.Second
const ftpDirectoryCacheLimit = 128
const ftpCacheBytes = 32 * 1024 * 1024
const ftpCacheDirectoryEntries = 32768

type ftpCacheToken struct {
	epoch   uint64
	started time.Time
}
type ftpCacheValue struct {
	key       string
	info      os.FileInfo
	entries   []os.FileInfo
	expires   time.Time
	bytes     int
	directory bool
}
type ftpCacheFlight struct {
	done  chan struct{}
	value ftpCacheValue
	err   error
}
type ftpCacheCounters struct{ Hits, Misses, Shared, Evictions, RemoteInvalidations uint64 }

// Cached slices are immutable. Each directory handle retains its own cursor.
type ftpMetadataCache struct {
	mu                             sync.Mutex
	entries                        map[string]*list.Element
	lru                            list.List
	flights                        map[string]*ftpCacheFlight
	epoch                          uint64
	writers                        int
	bytes, stats, dirs, dirEntries int
	counters                       ftpCacheCounters
}

func (c *ftpMetadataCache) resetLocked() {
	c.epoch++
	c.entries = nil
	c.lru.Init()
	c.bytes, c.stats, c.dirs, c.dirEntries = 0, 0, 0, 0
	// Existing callers still own their flights; new callers must not join them.
	c.flights = nil
}
func (c *ftpMetadataCache) snapshot() ftpCacheToken {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ftpCacheToken{c.epoch, time.Now()}
}
func (c *ftpMetadataCache) removeLocked(e *list.Element) {
	v := e.Value.(ftpCacheValue)
	delete(c.entries, v.key)
	c.lru.Remove(e)
	c.bytes -= v.bytes
	if v.directory {
		c.dirs--
		c.dirEntries -= len(v.entries)
	} else {
		c.stats--
	}
}
func (c *ftpMetadataCache) putLocked(token ftpCacheToken, v ftpCacheValue) {
	if c.writers != 0 || c.epoch != token.epoch || !time.Now().Before(v.expires) || v.bytes > ftpCacheBytes || len(v.entries) > ftpCacheDirectoryEntries {
		return
	}
	if e := c.entries[v.key]; e != nil {
		if e.Value.(ftpCacheValue).expires.After(v.expires) {
			return
		}
		c.removeLocked(e)
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[v.key] = c.lru.PushFront(v)
	c.bytes += v.bytes
	if v.directory {
		c.dirs++
		c.dirEntries += len(v.entries)
	} else {
		c.stats++
	}
	for c.bytes > ftpCacheBytes || c.stats > ftpMetadataLimit || c.dirs > ftpDirectoryCacheLimit || c.dirEntries > ftpCacheDirectoryEntries {
		c.removeLocked(c.lru.Back())
		c.counters.Evictions++
	}
}
func (c *ftpMetadataCache) load(key string, ttl time.Duration, fetch func() (ftpCacheValue, error)) (ftpCacheValue, error) {
	c.mu.Lock()
	token := ftpCacheToken{c.epoch, time.Now()}
	if e := c.entries[key]; e != nil {
		v := e.Value.(ftpCacheValue)
		if token.started.Before(v.expires) {
			c.lru.MoveToFront(e)
			c.counters.Hits++
			c.mu.Unlock()
			return v, nil
		}
		c.removeLocked(e)
	}
	if f := c.flights[key]; f != nil && c.writers == 0 {
		c.counters.Shared++
		c.mu.Unlock()
		<-f.done
		return f.value, f.err
	}
	c.counters.Misses++
	flight := &ftpCacheFlight{done: make(chan struct{})}
	if c.writers == 0 {
		if c.flights == nil {
			c.flights = make(map[string]*ftpCacheFlight)
		}
		c.flights[key] = flight
	}
	c.mu.Unlock()
	v, err := fetch()
	v.key = key
	v.expires = token.started.Add(ttl)
	c.mu.Lock()
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
	if err == nil {
		c.putLocked(token, v)
	}
	flight.value, flight.err = v, err
	close(flight.done)
	c.mu.Unlock()
	return v, err
}
func (c *ftpMetadataCache) stat(name string, fetch func() (os.FileInfo, error)) (os.FileInfo, error) {
	v, err := c.load("s:"+name, ftpMetadataTTL, func() (ftpCacheValue, error) {
		info, err := fetch()
		return ftpCacheValue{info: info, bytes: 256 + len(name) + ftpInfoBytes(info)}, err
	})
	return v.info, err
}
func (c *ftpMetadataCache) readDir(name string, fetch func() ([]os.FileInfo, error)) ([]os.FileInfo, error) {
	v, err := c.load("d:"+name, ftpDirectoryTTL, func() (ftpCacheValue, error) {
		entries, err := fetch()
		size := 256 + len(name)
		for _, info := range entries {
			size += ftpInfoBytes(info)
		}
		return ftpCacheValue{entries: entries, directory: true, bytes: size}, err
	})
	return v.entries, err
}
func (c *ftpMetadataCache) directory(token ftpCacheToken, name string, entries []os.FileInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, info := range entries[:min(len(entries), ftpMetadataLimit)] {
		if info.Mode()&os.ModeSymlink != 0 || safeName(info.Name()) != nil {
			continue
		}
		key := "s:" + path.Join(name, info.Name())
		c.putLocked(token, ftpCacheValue{key: key, info: info, expires: token.started.Add(ftpMetadataTTL), bytes: 256 + len(key) + ftpInfoBytes(info)})
	}
}
func (c *ftpMetadataCache) beginMutation() func() {
	c.mu.Lock()
	c.writers++
	c.resetLocked()
	c.mu.Unlock()
	return func() { c.mu.Lock(); c.writers--; c.resetLocked(); c.mu.Unlock() }
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
