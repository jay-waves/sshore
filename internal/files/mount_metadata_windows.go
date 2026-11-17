//go:build windows

package files

import (
	"os"
	"sync"
	"time"

	"github.com/winfsp/go-winfsp"
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

func (a *ftpWindowsAttributes) handleBehaviour(h uintptr) ftpWindowsBehaviour {
	a.namesMu.Lock()
	metadata := a.metadataHandles[h]
	a.namesMu.Unlock()
	if metadata {
		return a.metadata
	}
	return a.ftpWindowsBehaviour
}
func (a *ftpWindowsAttributes) Cleanup(ref *winfsp.FileSystemRef, h uintptr, name string, flags uint32) {
	a.handleBehaviour(h).Cleanup(ref, h, name, flags)
}
func (a *ftpWindowsAttributes) Flush(ref *winfsp.FileSystemRef, h uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	return a.handleBehaviour(h).Flush(ref, h, info)
}
func (a *ftpWindowsAttributes) GetSecurity(ref *winfsp.FileSystemRef, h uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	return a.handleBehaviour(h).GetSecurity(ref, h)
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
	name     string
	file     *ftpFile
	revision uint64
	expires  time.Time
	ready    chan struct{}
	err      error
	refs     int
	retained bool
	timer    *time.Timer
}
type ftpWindowsMetadataPool struct {
	backend *ftpFS
	mu      sync.Mutex
	entries map[string]*ftpWindowsMetadataEntry
	all     map[*ftpWindowsMetadataEntry]struct{}
	closing sync.WaitGroup
	opening sync.WaitGroup
	stopped bool
}

func (p *ftpWindowsMetadataPool) revision() uint64 {
	p.backend.cache.mu.Lock()
	defer p.backend.cache.mu.Unlock()
	return p.backend.cache.handleEpoch
}
func (p *ftpWindowsMetadataPool) acquire(name string) (gofs.File, error) {
	for {
		revision := p.revision()
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return nil, windows.STATUS_INVALID_HANDLE
		}
		e := p.entries[name]
		if e != nil && (e.revision != revision || !time.Now().Before(e.expires)) {
			p.mu.Unlock()
			p.retire(e)
			continue
		}
		if e != nil {
			// Pin before waiting, so expiry cannot close a handle being acquired.
			e.refs++
			p.mu.Unlock()
			<-e.ready
			p.mu.Lock()
			file, err := e.file, e.err
			if p.stopped {
				err = os.ErrClosed
			}
			p.mu.Unlock()
			if err != nil {
				p.release(e)
				return nil, ftpWindowsError(err)
			}
			return &ftpWindowsMetadataLease{ftpWindowsFile: &ftpWindowsFile{ftpFile: file}, pool: p, entry: e}, nil
		}
		e = &ftpWindowsMetadataEntry{name: name, revision: revision, expires: time.Now().Add(ftpWindowsMetadataHandleTTL), ready: make(chan struct{}), refs: 1}
		if p.all == nil {
			p.all = make(map[*ftpWindowsMetadataEntry]struct{})
		}
		p.all[e] = struct{}{}
		p.opening.Add(1)
		// A full pool still serves the open, but retains no extra idle descriptor.
		if len(p.entries) < ftpWindowsMetadataHandles {
			e.retained = true
			p.entries[name] = e
		}
		p.mu.Unlock()
		file, err := p.backend.openWithMetadata(name, os.O_RDONLY, 0, true)
		p.mu.Lock()
		if p.stopped && err == nil {
			err = os.ErrClosed
		}
		e.file, e.err = file, err
		close(e.ready)
		if e.retained {
			e.timer = time.AfterFunc(max(time.Until(e.expires), 0), func() { p.retire(e) })
		}
		p.mu.Unlock()
		if err != nil {
			p.retire(e)
			p.release(e)
			p.opening.Done()
			return nil, ftpWindowsError(err)
		}
		p.opening.Done()
		return &ftpWindowsMetadataLease{ftpWindowsFile: &ftpWindowsFile{ftpFile: file}, pool: p, entry: e}, nil
	}
}
func (p *ftpWindowsMetadataPool) retire(e *ftpWindowsMetadataEntry) {
	p.mu.Lock()
	if p.entries[e.name] == e {
		delete(p.entries, e.name)
	}
	e.retained = false
	if e.timer != nil {
		e.timer.Stop()
	}
	file := p.closedFileLocked(e)
	p.mu.Unlock()
	if file != nil {
		p.closeFile(file)
	}
}
func (p *ftpWindowsMetadataPool) closedFileLocked(e *ftpWindowsMetadataEntry) *ftpFile {
	if !p.stopped && (e.retained || e.refs != 0) {
		return nil
	}
	file := e.file
	e.file = nil
	delete(p.all, e)
	if file != nil {
		p.closing.Add(1)
	}
	return file
}
func (p *ftpWindowsMetadataPool) release(e *ftpWindowsMetadataEntry) {
	p.mu.Lock()
	e.refs--
	file := p.closedFileLocked(e)
	p.mu.Unlock()
	if file != nil {
		p.closeFile(file)
	}
}
func (p *ftpWindowsMetadataPool) close() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.opening.Wait()
	p.mu.Lock()
	entries := make([]*ftpWindowsMetadataEntry, 0, len(p.all))
	for e := range p.all {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	for _, e := range entries {
		p.retire(e)
	}
	p.closing.Wait()
}
func (p *ftpWindowsMetadataPool) closeFile(file *ftpFile) { defer p.closing.Done(); _ = file.Close() }

type ftpWindowsMetadataLease struct {
	*ftpWindowsFile
	pool  *ftpWindowsMetadataPool
	entry *ftpWindowsMetadataEntry
	once  sync.Once
}

func (f *ftpWindowsMetadataLease) Close() error {
	f.once.Do(func() { f.pool.release(f.entry) })
	return nil
}
func (f *ftpWindowsMetadataLease) Sync() error { return nil }

// These leases are never data handles, even if a caller bypasses native access checks.
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
