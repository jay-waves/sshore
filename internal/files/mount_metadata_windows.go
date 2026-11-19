//go:build windows

package files

import (
	"os"
	"sync"
	"time"

	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

// Attribute-only opens share short-lived, real SFTP handles. Each lease pins
// its file identity even after expiry, rename or replacement. Data/mutation
// access always goes through the ordinary adapter and opens a fresh handle.
func ftpWindowsMetadataOpen(opts, access uint32) bool {
	const allowed = windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	const options = windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_SYNCHRONOUS_IO_ALERT | windows.FILE_DIRECTORY_FILE | windows.FILE_NON_DIRECTORY_FILE
	return opts>>24 == windows.FILE_OPEN && opts&0xffffff & ^uint32(options) == 0 && access&windows.FILE_READ_ATTRIBUTES != 0 && access & ^uint32(allowed) == 0
}

type ftpWindowsMetadataFS struct {
	*ftpWindowsFS
	pool *ftpWindowsMetadataPool
}

func (f *ftpWindowsMetadataFS) OpenFile(name string, flags int, mode os.FileMode) (gofs.File, error) {
	if flags != os.O_RDONLY {
		return nil, windows.STATUS_ACCESS_DENIED
	}
	name, err := ftpWindowsPath(name)
	if err != nil {
		return nil, err
	}
	return f.pool.acquire(name)
}

const ftpWindowsMetadataHandles = 64

// Handle reuse has its own bounded lifetime; attribute freshness is independent.
const ftpWindowsMetadataHandleTTL = 5 * time.Second

type ftpWindowsMetadataEntry struct {
	name    string
	file    *ftpFile // Immutable once ready closes.
	epoch   uint64
	expires time.Time
	ready   chan struct{}
	err     error
	refs    int
}
type ftpWindowsMetadataPool struct {
	backend *ftpFS
	mu      sync.Mutex
	entries map[string]*ftpWindowsMetadataEntry
	all     map[*ftpWindowsMetadataEntry]struct{}
	timer   *time.Timer
	io      sync.WaitGroup
	stopped bool
}

func (p *ftpWindowsMetadataPool) acquire(name string) (gofs.File, error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, windows.STATUS_INVALID_HANDLE
	}
	// Read the epoch under the pool lock so a delayed caller cannot retire an
	// entry another caller already opened in a newer epoch.
	epoch, now := p.backend.cache.handleEpoch.Load(), time.Now()
	e := p.entries[name]
	var retired *ftpFile
	if e != nil && (e.epoch != epoch || !now.Before(e.expires)) {
		retired = p.retireLocked(e)
		e = nil
	}
	opening := e == nil
	if opening {
		e = &ftpWindowsMetadataEntry{name: name, epoch: epoch, expires: now.Add(ftpWindowsMetadataHandleTTL), ready: make(chan struct{})}
		if p.all == nil {
			p.all = make(map[*ftpWindowsMetadataEntry]struct{})
		}
		p.all[e] = struct{}{}
		p.io.Add(1)
		// A full pool serves the open without retaining an idle descriptor.
		if len(p.entries) < ftpWindowsMetadataHandles {
			p.entries[name] = e
			if p.timer == nil {
				p.timer = time.AfterFunc(time.Until(e.expires), p.expire)
			}
		}
	}
	// Pin before unlocking so expiry cannot close a handle being acquired.
	e.refs++
	p.mu.Unlock()

	if retired != nil {
		p.closeFile(retired)
	}
	if opening {
		p.open(e)
	} else {
		<-e.ready
	}
	p.mu.Lock()
	err := e.err
	if p.stopped {
		err = os.ErrClosed
	}
	p.mu.Unlock()
	if err != nil {
		p.release(e)
		return nil, ftpWindowsError(err)
	}
	return &ftpWindowsMetadataLease{pool: p, entry: e}, nil
}

func (p *ftpWindowsMetadataPool) open(e *ftpWindowsMetadataEntry) {
	defer p.io.Done()
	file, err := p.backend.openWithMetadata(e.name, os.O_RDONLY, 0, true)
	p.mu.Lock()
	if p.stopped {
		close(e.ready)
		p.mu.Unlock()
		// Shutdown already detached the entry. The opener still owns this file.
		if file != nil {
			_ = file.Close()
		}
		return
	}
	e.file, e.err = file, err
	if err != nil {
		p.retireLocked(e)
	}
	close(e.ready)
	p.mu.Unlock()
}
func (p *ftpWindowsMetadataPool) retireLocked(e *ftpWindowsMetadataEntry) *ftpFile {
	if p.entries[e.name] == e {
		delete(p.entries, e.name)
	}
	return p.takeFileLocked(e)
}

// One timer covers the retained handles. New entries expire no earlier than
// the timer already armed by an older entry.
func (p *ftpWindowsMetadataPool) expire() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	var next time.Time
	var files []*ftpFile
	now := time.Now()
	for _, e := range p.entries {
		if !now.Before(e.expires) {
			if file := p.retireLocked(e); file != nil {
				files = append(files, file)
			}
		} else if next.IsZero() || e.expires.Before(next) {
			next = e.expires
		}
	}
	if next.IsZero() {
		p.timer = nil
	} else {
		p.timer.Reset(time.Until(next))
	}
	p.mu.Unlock()
	for _, file := range files {
		p.closeFile(file)
	}
}

func (p *ftpWindowsMetadataPool) takeFileLocked(e *ftpWindowsMetadataEntry) *ftpFile {
	if _, owned := p.all[e]; !owned {
		return nil
	}
	if !p.stopped && (p.entries[e.name] == e || e.refs != 0) {
		return nil
	}
	// Removing the entry transfers close ownership. Leases keep pointing to
	// the original file object, including after shutdown closes it.
	delete(p.all, e)
	if e.file != nil {
		p.io.Add(1)
	}
	return e.file
}
func (p *ftpWindowsMetadataPool) release(e *ftpWindowsMetadataEntry) {
	p.mu.Lock()
	e.refs--
	file := p.takeFileLocked(e)
	p.mu.Unlock()
	if file != nil {
		p.closeFile(file)
	}
}
func (p *ftpWindowsMetadataPool) close() {
	p.mu.Lock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	// Detach every published file before waiting. Pending opens close their
	// own files without publishing, so no work can be added after this pass.
	var files []*ftpFile
	for e := range p.all {
		if file := p.takeFileLocked(e); file != nil {
			files = append(files, file)
		}
	}
	clear(p.entries)
	p.mu.Unlock()
	for _, file := range files {
		p.closeFile(file)
	}
	p.io.Wait()
}
func (p *ftpWindowsMetadataPool) closeFile(file *ftpFile) { defer p.io.Done(); _ = file.Close() }

type ftpWindowsMetadataLease struct {
	pool  *ftpWindowsMetadataPool
	entry *ftpWindowsMetadataEntry
	once  sync.Once
}

func (f *ftpWindowsMetadataLease) Close() error {
	f.once.Do(func() { f.pool.release(f.entry) })
	return nil
}
func (f *ftpWindowsMetadataLease) Sync() error { return nil }

func (f *ftpWindowsMetadataLease) Stat() (os.FileInfo, error) {
	info, err := f.entry.file.cachedStat()
	return info, ftpWindowsError(err)
}

// These leases are never data handles, even if a caller bypasses native access checks.
func (f *ftpWindowsMetadataLease) Seek(int64, int) (int64, error) {
	return 0, windows.STATUS_ACCESS_DENIED
}
func (f *ftpWindowsMetadataLease) Read([]byte) (int, error) { return 0, windows.STATUS_ACCESS_DENIED }
func (f *ftpWindowsMetadataLease) ReadAt([]byte, int64) (int, error) {
	return 0, windows.STATUS_ACCESS_DENIED
}
func (f *ftpWindowsMetadataLease) Write([]byte) (int, error) { return 0, windows.STATUS_ACCESS_DENIED }
func (f *ftpWindowsMetadataLease) WriteAt([]byte, int64) (int, error) {
	return 0, windows.STATUS_ACCESS_DENIED
}
func (f *ftpWindowsMetadataLease) Truncate(int64) error { return windows.STATUS_ACCESS_DENIED }
func (f *ftpWindowsMetadataLease) Readdir(int) ([]os.FileInfo, error) {
	return nil, windows.STATUS_ACCESS_DENIED
}
