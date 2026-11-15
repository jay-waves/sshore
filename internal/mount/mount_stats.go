package mount

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type ftpRequestTotals struct {
	Started, Completed, Errors uint64
	SentBytes, ReceivedBytes   uint64
	TotalNS, MaxNS             int64
}
type ftpCallbackTotals struct {
	Calls          uint64
	TotalNS, MaxNS int64
}
type ftpCallbackMetrics struct {
	mu     sync.Mutex
	totals map[string]ftpCallbackTotals
}

func (m *ftpCallbackMetrics) observe(name string) func() {
	if m == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		elapsed := int64(time.Since(start))
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.totals == nil {
			m.totals = make(map[string]ftpCallbackTotals)
		}
		v := m.totals[name]
		v.Calls++
		v.TotalNS += elapsed
		v.MaxNS = max(v.MaxNS, elapsed)
		m.totals[name] = v
	}
}
func writeFTPStats(w *ftpWire, f *ftpFS, out io.Writer) {
	report := struct {
		Type                                                       string
		Requests                                                   map[string]ftpRequestTotals
		Inflight, PeakInflight                                     int
		Cache                                                      ftpCacheCounters
		CacheBytes, MetadataEntries, Directories, DirectoryEntries int
		Callbacks                                                  map[string]ftpCallbackTotals
	}{Type: "mount_stats", Requests: make(map[string]ftpRequestTotals), Callbacks: make(map[string]ftpCallbackTotals)}
	w.mu.Lock()
	for k, v := range w.totals {
		if v.Started != 0 {
			report.Requests[ftpRequestName(byte(k))] = v
		}
	}
	report.Inflight, report.PeakInflight = len(w.requests), w.peakRequests
	w.mu.Unlock()
	f.cache.mu.Lock()
	report.Cache = f.cache.counters
	report.CacheBytes, report.MetadataEntries, report.Directories, report.DirectoryEntries = f.cache.bytes, f.cache.stats, f.cache.dirs, f.cache.dirEntries
	f.cache.mu.Unlock()
	if f.metrics != nil {
		f.metrics.mu.Lock()
		for k, v := range f.metrics.totals {
			report.Callbacks[k] = v
		}
		f.metrics.mu.Unlock()
	}
	_ = json.NewEncoder(out).Encode(report)
}
func startFTPStats(w *ftpWire, f *ftpFS, out io.Writer) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				writeFTPStats(w, f, out)
				return
			case <-ticker.C:
				writeFTPStats(w, f, out)
			}
		}
	}()
	return func() { close(stop); <-done }
}

func ftpRequestName(kind byte) string {
	names := map[byte]string{3: "OPEN", 4: "CLOSE", 5: "READ", 6: "WRITE", 7: "LSTAT", 8: "FSTAT", 9: "SETSTAT", 10: "FSETSTAT", 11: "OPENDIR", 12: "READDIR", 13: "REMOVE", 14: "MKDIR", 15: "RMDIR", 16: "REALPATH", 17: "STAT", 18: "RENAME", 19: "READLINK", 20: "SYMLINK", 200: "EXTENDED"}
	if name, ok := names[kind]; ok {
		return name
	}
	return fmt.Sprintf("TYPE_%d", kind)
}
