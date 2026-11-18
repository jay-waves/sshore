package files

import (
	"container/list"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

const ftpWatchLimit = 128
const ftpWatchTimeout = 3 * time.Second

// Watches are hints: TTLs remain mandatory for unsupported filesystems, symlink
// retargeting and mutations through other hard links. Never cache file contents.
type ftpNotifications struct {
	stream     *smux.Stream
	cache      *ftpMetadataCache
	mu         sync.Mutex // serializes subscription requests; the reader never takes it
	dirs       map[string]*list.Element
	lru        list.List
	replies    chan message
	done       chan struct{}
	generation atomic.Uint64
	closed     atomic.Bool
	warnOnce   sync.Once
	queueMu    sync.Mutex
	pending    map[string]bool
	queue      chan string
	workerDone chan struct{}
}

type ftpWatchEntry struct {
	name       string
	generation uint64
	expires    time.Time
	confirmed  bool
}

func startFTPNotifications(s *smux.Stream, cache *ftpMetadataCache) *ftpNotifications {
	n := &ftpNotifications{stream: s, cache: cache, dirs: make(map[string]*list.Element), replies: make(chan message, 1), done: make(chan struct{}), pending: make(map[string]bool), queue: make(chan string, ftpWatchLimit), workerDone: make(chan struct{})}
	go n.read()
	go n.subscribe()
	return n
}
func (n *ftpNotifications) warn() {
	n.warnOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "File notifications unavailable; using short-lived metadata caches.")
	})
}
func (n *ftpNotifications) read() {
	defer close(n.done)
	defer func() {
		if !n.closed.Load() {
			n.warn()
		}
		n.stop()
	}()
	for {
		m, err := readMessage(n.stream)
		if err != nil {
			return
		}
		switch m.Type {
		case "watch_ack":
			select {
			case n.replies <- m:
			default:
				return
			}
		case "watch_dirty":
			if m.Directory == "" {
				n.cache.invalidateRemote()
			} else if validFTPWatchPath(m.Directory) {
				n.cache.invalidateDirectory(m.Directory)
			} else {
				return
			}
		case "watch_reset":
			n.cache.invalidateRemote()
			n.generation.Add(1)
		default:
			return
		}
	}
}
func (n *ftpNotifications) stop() {
	if !n.closed.Swap(true) {
		n.cache.invalidateRemote()
		_ = n.stream.SetReadDeadline(time.Now())
		// smux Close can wait up to 30s to send FIN on a blocked transport.
		// Retire this reader immediately without delaying filesystem callbacks.
		go n.stream.Close()
	}
}
func (n *ftpNotifications) Close() { n.stop(); <-n.done; <-n.workerDone }

// A slow notification channel must not serialize filesystem reads. The queue
// bounds pending work; overflow leaves ordinary TTL caching in effect.
func (n *ftpNotifications) enqueue(dir string) {
	if n == nil || n.closed.Load() {
		return
	}
	n.queueMu.Lock()
	defer n.queueMu.Unlock()
	if n.pending[dir] {
		return
	}
	select {
	case n.queue <- dir:
		n.pending[dir] = true
	default:
	}
}
func (n *ftpNotifications) subscribe() {
	defer close(n.workerDone)
	for {
		select {
		case <-n.done:
			return
		case dir := <-n.queue:
			n.ensure(dir)
			n.queueMu.Lock()
			delete(n.pending, dir)
			n.queueMu.Unlock()
		}
	}
}

func (n *ftpNotifications) ensure(dir string) {
	if n == nil || n.closed.Load() {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed.Load() {
		return
	}
	generation := n.generation.Load()
	continuous := false
	if e := n.dirs[dir]; e != nil {
		v := e.Value.(ftpWatchEntry)
		continuous = v.confirmed && v.generation == generation
		if v.generation == generation && time.Now().Before(v.expires) {
			n.lru.MoveToFront(e)
			return
		}
		n.lru.Remove(e)
		delete(n.dirs, dir)
	}
	remove := ""
	if n.lru.Len() >= ftpWatchLimit {
		e := n.lru.Back()
		remove = e.Value.(ftpWatchEntry).name
		delete(n.dirs, remove)
		n.lru.Remove(e)
	}
	// One deadline covers both write and ack;
	// closing this stream only disables notifications, never file transfers.
	deadline := time.Now().Add(ftpWatchTimeout)
	_ = n.stream.SetWriteDeadline(deadline)
	err := writeMessage(n.stream, message{Type: "watch", Directory: dir, Name: remove})
	_ = n.stream.SetWriteDeadline(time.Time{})
	if err != nil {
		n.warn()
		n.stop()
		return
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case m := <-n.replies:
		// Reads can finish before registration. Drop their cached results and
		// invalidate in-flight fetch tokens to cover the unobserved interval.
		if !continuous || m.Error != "" || n.generation.Load() != generation {
			n.cache.registered(dir)
		}
		ttl := 30 * time.Second
		if m.Error != "" {
			n.warn()
			ttl = time.Second
		}
		n.dirs[dir] = n.lru.PushFront(ftpWatchEntry{name: dir, generation: generation, expires: time.Now().Add(ttl), confirmed: m.Error == ""})
	case <-timer.C:
		n.warn()
		n.stop()
	case <-n.done:
	}
}

func (f *ftpFS) watchDirectory(dir string) { f.notifications.enqueue(dir) }

func OpenMux(c io.ReadWriteCloser, version string, approval ...*MountApproval) (*smux.Session, *smux.Stream, *smux.Stream, error) {
	var source *MountApproval
	if len(approval) > 0 {
		source = approval[0]
	}
	return openMux(c, version, source, nil, nil)
}

func openMux(c io.ReadWriteCloser, version string, source *MountApproval, peer json.RawMessage, account *string) (*smux.Session, *smux.Stream, *smux.Stream, error) {
	if err := writeMessage(c, message{Type: "mount_mux", SoftwareVersion: version, Approval: source, PeerMetadata: peer}); err != nil {
		return nil, nil, nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = checkPeerVersion(version, m.SoftwareVersion, "remote mount agent"); err != nil {
		return nil, nil, nil, err
	}
	if m.Type != "mount_ready" {
		return nil, nil, nil, fmt.Errorf("remote mount agent: %s", m.Error)
	}
	if account != nil {
		*account = m.Account
	}
	session, err := smux.Client(c, muxConfig())
	if err != nil {
		return nil, nil, nil, err
	}
	data, err := session.OpenStream()
	if err != nil {
		session.Close()
		return nil, nil, nil, err
	}
	events, err := session.OpenStream()
	if err != nil {
		session.Close()
		return nil, nil, nil, err
	}
	return session, data, events, nil
}

func validFTPWatchPath(p string) bool {
	return len(p) <= 4096 && strings.HasPrefix(p, "/") && !strings.ContainsRune(p, 0) && path.Clean(p) == p
}

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
