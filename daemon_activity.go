package main

import (
	"context"
	"fmt"
	"iter"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

const daemonHeartbeatInterval = 20 * time.Second
const daemonHeartbeatTimeout = 75 * time.Second
const peerAdvertisementTTL = 24 * time.Hour
const maxPeerAdvertisements = 512

type activityRequest struct {
	Metadata  *metadataRequest
	Kind, Key string
	Peer      *peerRecord
	Owner     processIdentity
}
type peerAdvertisement struct {
	Record peerRecord
	Seen   time.Time
}
type activityReply struct {
	Metadata *metadataReply
	Peers    []peerAdvertisement
	Error    string
}
type activityLease struct {
	shellPath string
	peer      *peerRecord
	owner     processIdentity
	expired   chan struct{}
}

// All live advertisements and leases belong to the account daemon. No state
// file or PID scan can recreate a lease after its connection has gone away.
type daemonActivity struct {
	root      string
	tasks     *daemonTasks
	metadata  daemonMetadata
	mu        sync.Mutex
	leases    map[*activityLease]struct{}
	peers     map[string]peerAdvertisement
	pulse     chan struct{}
	lastPulse time.Time
}

var accountActivity atomic.Pointer[daemonActivity]

func (s *daemonActivity) registerLease(request activityRequest) (*activityLease, error) {
	if (request.Kind != "incoming" && request.Peer != nil) || (request.Kind != "owner" && request.Owner != (processIdentity{})) {
		return nil, fmt.Errorf("unexpected activity lease metadata")
	}
	l := &activityLease{}
	switch request.Kind {
	case "shell":
		l.shellPath = request.Key
	case "heartbeat":
	case "incoming":
		if request.Peer == nil {
			return nil, fmt.Errorf("invalid incoming task")
		}
		peer := *request.Peer
		if validateRemoteTask(peer) != nil || peer.State != "active" {
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
	s.leases[l] = struct{}{}
	return l, nil
}
func (s *daemonActivity) remove(l *activityLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, l)
}

func (s *daemonActivity) activeShellPaths() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make(map[string]bool)
	for l := range s.leases {
		if l.shellPath != "" {
			paths[l.shellPath] = true
		}
	}
	return paths
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

// The caller holds the activity lock. All readers and admission checks use the
// same expiry rule, including when the housekeeping tick has not run yet.
func (s *daemonActivity) livePeersLocked(now time.Time) iter.Seq2[string, peerAdvertisement] {
	return func(yield func(string, peerAdvertisement) bool) {
		for key, peer := range s.peers {
			if now.Sub(peer.Seen) > peerAdvertisementTTL {
				delete(s.peers, key)
				continue
			}
			if !yield(key, peer) {
				return
			}
		}
	}
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
	for range s.livePeersLocked(now) {
	}
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
func (s *daemonActivity) request(r activityRequest) activityReply {
	if r.Kind == "metadata" && r.Metadata != nil {
		reply := s.handleMetadata(*r.Metadata)
		return activityReply{Metadata: &reply}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Kind {
	case "peer_put":
		if r.Peer == nil {
			return activityReply{Error: "peer metadata missing"}
		}
		if err := validateRemoteTask(*r.Peer); err != nil {
			return activityReply{Error: err.Error()}
		}
		if s.peers == nil {
			s.peers = make(map[string]peerAdvertisement)
		}
		now := time.Now()
		if _, exists := s.peers[r.Peer.Key]; !exists && len(s.peers) >= maxPeerAdvertisements {
			for range s.livePeersLocked(now) {
			}
			if len(s.peers) >= maxPeerAdvertisements {
				return activityReply{Error: "peer inventory is full"}
			}
		}
		s.peers[r.Peer.Key] = peerAdvertisement{Record: *r.Peer, Seen: now}
	case "peer_remove":
		if !validRequestID(r.Key) {
			return activityReply{Error: "invalid peer task key"}
		}
		delete(s.peers, r.Key)
	case "list", "incoming":
		reply := activityReply{}
		now := time.Now()
		liveOnly := r.Kind == "incoming"
		peers := map[string]peerAdvertisement{}
		for l := range s.leases {
			if peer := l.peer; peer != nil {
				peers[peer.Key] = peerAdvertisement{Record: *peer, Seen: now}
			}
		}
		for key, p := range s.livePeersLocked(now) {
			live, leased := peers[key]
			if leased && live.Record.UID == p.Record.UID && p.Record.State == "active" {
				p.Seen = now
			} else if liveOnly {
				// Keep the lease's own record when the advertisement disagrees.
				continue
			}
			peers[key] = p
		}
		for _, peer := range peers {
			reply.Peers = append(reply.Peers, peer)
		}
		return reply
	default:
		return activityReply{Error: "invalid activity operation"}
	}
	return activityReply{}
}
func activityRPC(r activityRequest) (activityReply, error) {
	if !activityAllowed() {
		readOnly := r.Kind == "list" || r.Kind == "incoming" || r.Kind == "metadata" && r.Metadata != nil && (r.Metadata.Action == "tree_get" || r.Metadata.Action == "tree_list" || r.Metadata.Action == "shell_get" || r.Metadata.Action == "connection_get" || r.Metadata.Action == "connection_select")
		if !readOnly {
			return activityReply{}, fmt.Errorf("daemon activity is disabled in recovery mode")
		}
		if _, err := existingDaemonRecord(); os.IsNotExist(err) {
			return activityReply{Metadata: &metadataReply{}}, nil
		}
	}

	if s := accountActivity.Load(); s != nil {
		reply := s.request(r)
		if reply.Error != "" {
			return reply, fmt.Errorf("%s", reply.Error)
		}
		return reply, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := activityDaemon(ctx)
	var reply activityReply
	if err == nil {
		err = daemonJSON(ctx, record, "/activity", r, &reply)
	}
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply, err
}
func openActivity(parent context.Context, r activityRequest) (net.Conn, error) {
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
func (s *daemonActivity) serve(c net.Conn, r activityRequest) {
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
	var requestedRelease bool // Published by closing done.
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
				requestedRelease = m.Type == "release"
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			if requestedRelease {
				_ = sendLeaseMessage(c, "released")
			}
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
func leaseActivity(r activityRequest) (func(), error) {
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

func remoteActivity(ctx context.Context, r activityRequest) (func(), <-chan struct{}, error) {
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
	return leaseActivity(activityRequest{Kind: "heartbeat"})
}
func withDaemonActivity(execute func() (int, error)) (int, error) {
	stop, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stop()
	return execute()
}
