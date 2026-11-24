package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

const ftpWatchLimit = 128
const ftpWatchTimeout = 3 * time.Second

// Watches are hints: TTLs remain mandatory for unsupported filesystems, symlink
// retargeting and mutations through other hard links. Never cache file contents.
type ftpNotifications struct {
	stream        io.ReadWriteCloser
	cache         *ftpMetadataCache
	mu            sync.Mutex // protects subscription state, never held during transport I/O
	dirs          map[string]ftpWatchEntry
	recency       uint64
	generation    uint64
	registration  ftpWatchRegistration
	done          chan struct{}
	warnOnce      sync.Once
	onUnavailable func()
	queue         chan string
	workers       sync.WaitGroup
}

type ftpWatchRegistration struct {
	dir        string
	generation uint64
	continuous bool
}

type ftpWatchEntry struct {
	generation uint64
	lastUse    uint64
	expires    time.Time
	confirmed  bool
	ready      chan struct{} // Non-nil while a registration is queued or in flight.
}

func startFTPNotifications(s io.ReadWriteCloser, cache *ftpMetadataCache, onUnavailable func()) *ftpNotifications {
	n := &ftpNotifications{stream: s, cache: cache, onUnavailable: onUnavailable, dirs: make(map[string]ftpWatchEntry), done: make(chan struct{}), queue: make(chan string, ftpWatchLimit)}
	if s == nil {
		n.stop(true)
		return n
	}
	n.workers.Go(n.read)
	n.workers.Go(n.subscribe)
	return n
}
func (n *ftpNotifications) warn() {
	if n.onUnavailable != nil {
		n.warnOnce.Do(n.onUnavailable)
	}
}
func (n *ftpNotifications) read() {
	defer n.stop(true)
	for {
		m, err := readMessage(n.stream)
		if err != nil {
			return
		}
		switch m.Type {
		case "watch_ack":
			if !n.acknowledge(m.Error == "") {
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
			n.mu.Lock()
			n.generation++
			n.mu.Unlock()
			n.cache.invalidateRemote()
		default:
			return
		}
	}
}

// Process the acknowledgement in stream order with dirty and reset messages.
func (n *ftpNotifications) acknowledge(confirmed bool) bool {
	n.mu.Lock()
	request, generation := n.registration, n.generation
	n.registration = ftpWatchRegistration{}
	n.mu.Unlock()
	if request.dir == "" {
		return false
	}
	// Cover the unobserved interval before releasing any waiters.
	if !request.continuous || !confirmed || request.generation != generation {
		n.cache.registered(request.dir)
	}
	ttl := 30 * time.Second
	if !confirmed {
		n.warn()
		ttl = time.Second
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dirs == nil {
		return false
	}
	ready := n.dirs[request.dir].ready
	n.recency++
	n.dirs[request.dir] = ftpWatchEntry{generation: request.generation, lastUse: n.recency, expires: time.Now().Add(ttl), confirmed: confirmed}
	close(ready)
	return true
}

func (n *ftpNotifications) stop(warn bool) {
	n.mu.Lock()
	if n.dirs == nil {
		n.mu.Unlock()
		return
	}
	n.dirs = nil
	n.registration = ftpWatchRegistration{}
	n.mu.Unlock()

	if warn {
		n.warn()
	}
	n.cache.invalidateRemote()
	// All registration waiters also listen here. Their individual channels are
	// only for acknowledgements while the subscription table is still live.
	close(n.done)
	if n.stream == nil {
		return
	}
	if stream, ok := n.stream.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = stream.SetReadDeadline(time.Now())
	}
	// Close the endpoint without delaying filesystem callbacks.
	go n.stream.Close()
}
func (n *ftpNotifications) Close() {
	n.stop(false)
	<-n.done
	n.workers.Wait()
}

// A slow notification channel must not serialize filesystem reads. The queue
// bounds pending work; overflow leaves ordinary TTL caching in effect.
func (n *ftpNotifications) enqueue(dir string) <-chan struct{} {
	if n == nil || dir == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dirs == nil {
		return nil
	}
	entry := n.dirs[dir]
	if entry.ready != nil {
		return entry.ready
	}
	if entry.generation == n.generation && time.Now().Before(entry.expires) {
		n.recency++
		entry.lastUse = n.recency
		n.dirs[dir] = entry
		return nil
	}
	select {
	case n.queue <- dir:
		entry.ready = make(chan struct{})
		n.dirs[dir] = entry
		return entry.ready
	default:
		return nil
	}
}

func (n *ftpNotifications) ensure(ctx context.Context, dir string) {
	if done := n.enqueue(dir); done != nil {
		select {
		case <-done:
		case <-n.done:
		case <-ctx.Done():
		}
	}
}

func (n *ftpNotifications) subscribe() {
	timer := time.NewTimer(ftpWatchTimeout)
	timer.Stop()
	for {
		select {
		case <-n.done:
			return
		case dir := <-n.queue:
			n.register(dir, timer)
		}
	}
}

func (n *ftpNotifications) register(dir string, timer *time.Timer) {
	n.mu.Lock()
	previous := n.dirs[dir]
	if previous.ready == nil {
		n.mu.Unlock()
		return
	}
	remove := ""
	if len(n.dirs) > ftpWatchLimit {
		count := 0
		for name, entry := range n.dirs {
			if name == dir || entry.expires.IsZero() {
				continue
			}
			count++
			if remove == "" || entry.lastUse < n.dirs[remove].lastUse {
				remove = name
			}
		}
		if count < ftpWatchLimit {
			remove = ""
		} else if old := n.dirs[remove]; old.ready != nil {
			// Keep the queued renewal and any waiters after evicting its old watch.
			n.dirs[remove] = ftpWatchEntry{ready: old.ready}
		} else {
			delete(n.dirs, remove)
		}
	}
	n.registration = ftpWatchRegistration{
		dir: dir, generation: n.generation,
		continuous: previous.confirmed && previous.generation == n.generation,
	}
	n.mu.Unlock()
	// One deadline covers both write and ack;
	// closing this stream only disables notifications, never file transfers.
	deadline := time.Now().Add(ftpWatchTimeout)
	stopWrite := time.AfterFunc(time.Until(deadline), func() { n.stop(true) })
	err := writeMessage(n.stream, message{Type: "watch", Directory: dir, Name: remove})
	stopWrite.Stop()
	if err != nil {
		n.stop(true)
		return
	}
	timer.Reset(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-previous.ready:
			return
		case <-n.done:
			return
		case <-timer.C:
			n.mu.Lock()
			awaitingAck := n.registration.dir != ""
			n.mu.Unlock()
			if awaitingAck {
				n.stop(true)
				return
			}
			// The reader has the reply and is finishing the cache update.
		}
	}
}

func negotiateMount(c io.ReadWriter, mode, version string, source *MountApproval, peer json.RawMessage, account *string) error {
	if err := writeMessage(c, message{Type: mode, SoftwareVersion: version, Approval: source, PeerMetadata: peer}); err != nil {
		return err
	}
	m, err := readMessage(c)
	if err != nil {
		return err
	}
	if err = checkPeerVersion(version, m.SoftwareVersion, "remote mount agent"); err != nil {
		return err
	}
	if m.Type != "mount_ready" {
		return fmt.Errorf("remote mount agent: %s", m.Error)
	}
	if account != nil {
		*account = m.Account
	}
	return nil
}

func validFTPWatchPath(p string) bool {
	return len(p) <= 4096 && strings.HasPrefix(p, "/") && !strings.ContainsRune(p, 0) && path.Clean(p) == p
}

// Coalesce by directory while the network writer is busy. Lost detail always
// becomes a full reset rather than an incomplete invalidation list.
type watchChanges struct {
	dirs  map[string]struct{}
	reset bool
}

func (c *watchChanges) add(dir string, reset bool) {
	if c.reset {
		return
	}
	if !reset && validFTPWatchPath(dir) {
		if c.dirs == nil {
			c.dirs = make(map[string]struct{})
		}
		c.dirs[dir] = struct{}{}
		if len(c.dirs) <= ftpWatchLimit {
			return
		}
	}
	*c = watchChanges{reset: true}
}
