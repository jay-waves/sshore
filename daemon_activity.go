package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

const daemonHeartbeatInterval = 20 * time.Second
const daemonHeartbeatTimeout = 75 * time.Second

type leaseRequest struct {
	Kind  string
	Peer  *peerRecord
	Owner processIdentity
}
type activityLease struct {
	order   uint64
	peer    *peerRecord
	owner   processIdentity
	expired chan struct{}
}

// All live advertisements and leases belong to the account daemon. No state
// file or PID scan can recreate a lease after its connection has gone away.
type daemonActivity struct {
	root          string
	tasks         *daemonTasks
	metadata      daemonMetadata
	mu            sync.Mutex
	leases        map[*activityLease]struct{}
	nextPeerOrder uint64
	pulse         chan struct{}
	lastPulse     time.Time
}

var accountActivity atomic.Pointer[daemonActivity]

func (s *daemonActivity) registerLease(request leaseRequest) (*activityLease, error) {
	if (request.Kind != "incoming" && request.Peer != nil) || (request.Kind != "owner" && request.Owner != (processIdentity{})) {
		return nil, fmt.Errorf("unexpected activity lease metadata")
	}
	l := &activityLease{}
	switch request.Kind {
	case "heartbeat":
	case "incoming":
		if request.Peer == nil {
			return nil, fmt.Errorf("invalid incoming task")
		}
		peer := *request.Peer
		if validateRemoteTask(peer) != nil {
			return nil, fmt.Errorf("invalid incoming task")
		}
		l.peer = &peer
	case "owner":
		if request.Owner.PID <= 1 || request.Owner.Start == "" {
			return nil, fmt.Errorf("invalid activity owner")
		}
		l.owner = request.Owner
		l.expired = make(chan struct{})
	default:
		return nil, fmt.Errorf("invalid activity lease kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		s.leases = make(map[*activityLease]struct{})
	}
	if l.peer != nil {
		s.nextPeerOrder++
		l.order = s.nextPeerOrder
	}
	s.leases[l] = struct{}{}
	return l, nil
}
func (s *daemonActivity) remove(l *activityLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, l)
}

// The clock closes one channel per heartbeat. Each consumer subscribes to the
// next heartbeat before doing I/O so one can arrive while it is busy.
func (s *daemonActivity) heartbeat() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pulse == nil {
		s.pulse = make(chan struct{})
	}
	return s.pulse
}

func (s *daemonActivity) tick(now time.Time) {
	s.mu.Lock()
	if now.Sub(s.lastPulse) >= daemonHeartbeatInterval {
		s.lastPulse = now
		if s.pulse != nil {
			close(s.pulse)
			s.pulse = nil
		}
	}
	var owners []*activityLease
	for l := range s.leases {
		if l.owner.PID != 0 {
			owners = append(owners, l)
		}
	}
	s.mu.Unlock()
	if len(owners) == 0 {
		return
	}

	// Read each PID once outside the activity lock. Reuse the snapshot for
	// expired leases, then retire only those still registered.
	identities := make(map[int]string, len(owners))
	expired := owners[:0]
	for _, l := range owners {
		owner := l.owner
		identity, checked := identities[owner.PID]
		if !checked {
			var err error
			identity, err = proc.LiveIdentity(owner.PID)
			if err != nil {
				identity = "" // Registered owners always have a start identity.
			}
			identities[owner.PID] = identity
		}
		if identity != owner.Start {
			expired = append(expired, l)
		}
	}
	if len(expired) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range expired {
		if _, active := s.leases[l]; active {
			delete(s.leases, l)
			close(l.expired)
		}
	}
}

// Overlapping connections can describe the same resource. Show the newest
// live registration, including when an earlier connection closes out of order.
func (s *daemonActivity) incomingPeers() []peerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := make(map[string]*activityLease)
	for lease := range s.leases {
		if peer := lease.peer; peer != nil {
			if old := latest[peer.Key]; old == nil || old.order < lease.order {
				latest[peer.Key] = lease
			}
		}
	}
	peers := make([]peerRecord, 0, len(latest))
	for _, lease := range latest {
		peers = append(peers, *lease.peer)
	}
	return peers
}

func openActivity(parent context.Context, r leaseRequest) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	record, err := activityDaemon(ctx)
	if err != nil {
		return nil, err
	}
	return dialDaemonStream(ctx, record, time.Now().Add(10*time.Second), "/leases", r)
}
func activityDaemon(ctx context.Context) (localShellRecord, error) {
	if activityAllowed() {
		return ensureLocalDaemonContext(ctx)
	}
	return existingDaemonRecord()
}
func (s *daemonActivity) serve(c net.Conn, r leaseRequest) {
	l, err := s.registerLease(r)
	if err != nil {
		return
	}
	pulse := s.heartbeat()
	if writeMessage(c, message{Type: "activity_ready"}) != nil {
		s.remove(l)
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	done := make(chan struct{})
	defer func() {
		_ = c.Close()
		<-done
	}()
	go func() {
		defer close(done)
		// Read-side termination retires the lease even if a pulse write is blocked.
		defer s.remove(l)
		for {
			_ = c.SetReadDeadline(time.Now().Add(daemonHeartbeatTimeout))
			m, err := readMessage(c)
			if err != nil {
				return
			}
			if m.Type != "alive" {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-l.expired:
			return
		case <-pulse:
			pulse = s.heartbeat()
			if sendLeaseMessage(c, "pulse") != nil {
				return
			}
		}
	}
}

func sendLeaseMessage(c net.Conn, kind string) error {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.SetWriteDeadline(time.Time{})
	return writeMessage(c, message{Type: kind})
}

// A client has no polling timer. The daemon sends pulses and observes EOF or
// missed acknowledgements; closing this lease releases activity immediately.
func leaseActivity(r leaseRequest) (func(), error) {
	if s := accountActivity.Load(); s != nil {
		l, err := s.registerLease(r)
		if err != nil {
			return nil, err
		}
		return func() { s.remove(l) }, nil
	}
	stop, _, err := remoteActivity(context.Background(), r)
	return stop, err
}

func remoteActivity(ctx context.Context, r leaseRequest) (func(), <-chan struct{}, error) {
	c, err := openActivity(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	return acceptActivityLease(ctx, c)
}

// Shared lease supervision also keeps a direct spawn owner alive over SSH.
func acceptActivityLease(ctx context.Context, c net.Conn) (func(), <-chan struct{}, error) {
	closeConnection := sync.OnceFunc(func() { _ = c.Close() })
	stop := context.AfterFunc(ctx, closeConnection)
	ready, err := readMessage(c)
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case err != nil:
		err = fmt.Errorf("activity registration failed: %w", err)
	case ready.Type != "activity_ready":
		err = fmt.Errorf("unexpected activity registration reply: %s", ready.Type)
	}
	if err != nil {
		stop()
		closeConnection()
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Time{})
	var writeMu sync.Mutex
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer closeConnection()
		defer stop()
		for {
			_ = c.SetReadDeadline(time.Now().Add(daemonHeartbeatTimeout))
			// SSH forwarded channels do not implement read deadlines. Closing the
			// endpoint bounds a missing pulse on those transports as well.
			timeout := time.AfterFunc(daemonHeartbeatTimeout, closeConnection)
			m, err := readMessage(c)
			alive := timeout.Stop()
			if !alive || err != nil || m.Type != "pulse" {
				return
			}
			writeMu.Lock()
			err = sendLeaseMessage(c, "alive")
			writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return sync.OnceFunc(func() {
		select {
		case <-finished:
			return
		default:
		}
		writeMu.Lock()
		err := sendLeaseMessage(c, "release")
		writeMu.Unlock()
		if err == nil {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
			}
		}
		stop()
		_ = c.Close()
		<-finished
	}), finished, nil
}

func activityAllowed() bool { return os.Getenv("SSHORE_DISABLED") != "1" }

func (s *daemonActivity) busy() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.leases) != 0 }

func beginCommandActivity() (func(), error) {
	if !activityAllowed() {
		return func() {}, nil
	}
	return leaseActivity(leaseRequest{Kind: "heartbeat"})
}
func withDaemonActivity(execute func() (int, error)) (int, error) {
	stop, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stop()
	return execute()
}
