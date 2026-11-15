package main

import (
	"container/list"
	"fmt"
	"io"
	"os"
	"path"
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
}

type ftpWatchEntry struct {
	name       string
	generation uint64
	expires    time.Time
}

func startFTPNotifications(s *smux.Stream, cache *ftpMetadataCache) *ftpNotifications {
	n := &ftpNotifications{stream: s, cache: cache, dirs: make(map[string]*list.Element), replies: make(chan message, 1), done: make(chan struct{})}
	go n.read()
	return n
}
func (n *ftpNotifications) warn() {
	n.warnOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "Remote file notifications unavailable; mount continues with short-lived metadata caches.")
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
		case "watch_dirty", "watch_reset":
			n.cache.invalidateRemote()
			if m.Type == "watch_reset" {
				n.generation.Add(1)
			}
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
func (n *ftpNotifications) Close() { n.stop(); <-n.done }

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
	if e := n.dirs[dir]; e != nil {
		v := e.Value.(ftpWatchEntry)
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
	// Subscribe before fetching metadata. One deadline covers both write and ack;
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
		ttl := 30 * time.Second
		if m.Error != "" {
			n.warn()
			ttl = time.Second
		}
		n.dirs[dir] = n.lru.PushFront(ftpWatchEntry{dir, generation, time.Now().Add(ttl)})
	case <-timer.C:
		n.warn()
		n.stop()
	case <-n.done:
	}
}

func (f *ftpFS) watchDirectory(dir string) { f.notifications.ensure(dir) }

func mountSSHArgs(o ftpOptions) []string {
	if o.noWatch {
		return sftpSSHArgs(o.host, o.ssh)
	}
	server := o.server
	if server == "" {
		server = "sshore"
	}
	command := "unset SSHORE_SOCKET SSHORE_ASKPASS SSHORE_ASKPASS_ADDR SSHORE_ASKPASS_TOKEN; server=" + shellQuote(server) + "; if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); "
	if server == "sshore" {
		command += "else server=\"$HOME/.local/bin/sshore\"; "
	} else {
		command += "else :; "
	}
	command += "fi; if [ ! -x \"$server\" ]; then printf '%s\\n' 'sshore: mount notifications require remote sshore; install the matching release or use --no-watch' >&2; exit 1; fi; exec \"$server\" mount-agent"
	args := append(ownedSSHArgs("default"), "-o", "ClearAllForwardings=yes")
	args = append(args, o.ssh...)
	return append(args, "--", o.host, "sh -c "+shellQuote(command))
}

func openFTPMux(c io.ReadWriteCloser) (*smux.Session, *smux.Stream, *smux.Stream, error) {
	if err := writeMessage(c, message{Type: "mount_mux", SoftwareVersion: version}); err != nil {
		return nil, nil, nil, err
	}
	m, err := readMessage(c)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = checkPeerVersion(m.SoftwareVersion, "remote mount agent"); err != nil {
		return nil, nil, nil, err
	}
	if m.Type != "mount_ready" {
		return nil, nil, nil, fmt.Errorf("remote mount agent: %s", m.Error)
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
