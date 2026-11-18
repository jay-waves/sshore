package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const daemonHeartbeatInterval = 20 * time.Second
const daemonHeartbeatTimeout = 75 * time.Second

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
	Incoming []peerRecord
	Active   []string
	Error    string
}
type activityLease struct {
	request activityRequest
	pulse   chan struct{}
	expired chan struct{}
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
	lastPulse time.Time
}

var accountActivity atomic.Pointer[daemonActivity]

func (s *daemonActivity) add(request activityRequest) *activityLease {
	l := &activityLease{request: request, pulse: make(chan struct{}, 1), expired: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		s.leases = make(map[*activityLease]struct{})
	}
	s.leases[l] = struct{}{}
	return l
}
func (s *daemonActivity) remove(l *activityLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, l)
}
func (s *daemonActivity) tick(now time.Time) {
	s.mu.Lock()
	pulse := now.Sub(s.lastPulse) >= daemonHeartbeatInterval
	if pulse {
		s.lastPulse = now
	}
	var owners []*activityLease
	for key, p := range s.peers {
		if now.Sub(p.Seen) > 24*time.Hour {
			delete(s.peers, key)
		}
	}
	for l := range s.leases {
		if l.request.Owner.PID != 0 {
			owners = append(owners, l)
		}
		if pulse {
			select {
			case l.pulse <- struct{}{}:
			default:
			}
		}
	}
	s.mu.Unlock()
	for _, l := range owners {
		identity, err := spawnProcessIdentity(l.request.Owner.PID)
		if err != nil || identity != l.request.Owner.Start {
			s.mu.Lock()
			if _, active := s.leases[l]; active {
				delete(s.leases, l)
				close(l.expired)
			}
			s.mu.Unlock()
		}
	}
}
func (s *daemonActivity) request(r activityRequest) activityReply {
	if r.Kind == "metadata" && r.Metadata != nil {
		reply := s.metadata.handle(*r.Metadata)
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
		if len(s.peers) >= 512 {
			if _, ok := s.peers[r.Peer.Key]; !ok {
				return activityReply{Error: "peer inventory is full"}
			}
		}
		s.peers[r.Peer.Key] = peerAdvertisement{Record: *r.Peer, Seen: time.Now()}
	case "peer_remove":
		if !validRequestID(r.Key) {
			return activityReply{Error: "invalid peer task key"}
		}
		delete(s.peers, r.Key)
	case "list":
		reply := activityReply{}
		incoming := map[string]peerRecord{}
		for l := range s.leases {
			if l.request.Kind == "shell" {
				reply.Active = append(reply.Active, l.request.Key)
			}
			if l.request.Peer != nil {
				incoming[l.request.Peer.Key] = *l.request.Peer
			}
		}
		for key, p := range s.peers {
			if time.Since(p.Seen) > 24*time.Hour {
				delete(s.peers, key)
				continue
			}
			if previous, ok := incoming[key]; ok && previous.UID == p.Record.UID && p.Record.State == "active" {
				incoming[key] = p.Record
				p.Seen = time.Now()
			}
			reply.Peers = append(reply.Peers, p)
		}
		for key, p := range incoming {
			if _, ok := s.peers[key]; !ok {
				reply.Peers = append(reply.Peers, peerAdvertisement{Record: p, Seen: time.Now()})
			}
			reply.Incoming = append(reply.Incoming, p)
		}
		return reply
	default:
		return activityReply{Error: "invalid activity operation"}
	}
	return activityReply{}
}
func activityRPC(r activityRequest) (activityReply, error) {
	if !activityAllowed() {
		readOnly := r.Kind == "list" || r.Kind == "metadata" && r.Metadata != nil && (r.Metadata.Action == "tree_get" || r.Metadata.Action == "tree_list" || r.Metadata.Action == "shell_get" || r.Metadata.Action == "connection_get")
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
	c, err := openActivity(context.Background(), r)
	if err != nil {
		return activityReply{}, err
	}
	defer c.Close()
	var reply activityReply
	err = json.NewDecoder(c).Decode(&reply)
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply, err
}
func openActivity(parent context.Context, r activityRequest) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var record localShellRecord
	var err error
	if activityAllowed() {
		record, err = ensureLocalDaemonContext(ctx)
	} else {
		record, err = existingDaemonRecord()
	}
	if err != nil {
		return nil, err
	}
	c, err := dialLocalSessionContext(ctx, record, time.Now().Add(10*time.Second))
	if err != nil {
		return nil, err
	}
	if err = writeMessage(c, message{Type: "activity", Activity: &r}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (s *daemonActivity) serve(c net.Conn, r activityRequest) {
	if r.Kind != "shell" && r.Kind != "incoming" && r.Kind != "heartbeat" && r.Kind != "owner" {
		_ = json.NewEncoder(c).Encode(s.request(r))
		return
	}
	if r.Kind == "incoming" && (r.Peer == nil || validateRemoteTask(*r.Peer) != nil || r.Peer.State != "active") {
		return
	}
	if r.Kind == "owner" && (r.Owner.PID <= 1 || r.Owner.Start == "") {
		return
	}
	l := s.add(r)
	defer s.remove(l)
	if writeMessage(c, message{Type: "activity_ready"}) != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	received := make(chan string, 1)
	go func() {
		for {
			_ = c.SetReadDeadline(time.Now().Add(daemonHeartbeatTimeout))
			m, err := readMessage(c)
			if err != nil {
				received <- "closed"
				return
			}
			if m.Type != "alive" {
				received <- m.Type
				return
			}
		}
	}()
	for {
		select {
		case kind := <-received:
			s.remove(l)
			if kind == "release" {
				_ = writeMessage(c, message{Type: "released"})
			}
			return
		case <-l.expired:
			return
		case <-l.pulse:
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if writeMessage(c, message{Type: "pulse"}) != nil {
				return
			}
		}
	}
}

// A client has no polling timer. The daemon sends pulses and observes EOF or
// missed acknowledgements; closing this lease releases activity immediately.
func leaseActivity(r activityRequest, pulse func()) (func(), error) {
	if s := accountActivity.Load(); s != nil {
		if r.Kind == "incoming" && (r.Peer == nil || validateRemoteTask(*r.Peer) != nil || r.Peer.State != "active") {
			return nil, fmt.Errorf("invalid incoming task")
		}
		l := s.add(r)
		return func() { s.remove(l) }, nil
	}
	c, err := openActivity(context.Background(), r)
	if err != nil {
		return nil, err
	}
	ready, err := readMessage(c)
	if err != nil || ready.Type != "activity_ready" {
		c.Close()
		return nil, fmt.Errorf("activity registration failed: %v", err)
	}
	_ = c.SetDeadline(time.Time{})
	var writeMu sync.Mutex
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer c.Close()
		for {
			_ = c.SetReadDeadline(time.Now().Add(daemonHeartbeatTimeout))
			m, err := readMessage(c)
			if err != nil || m.Type != "pulse" {
				return
			}
			if pulse != nil {
				pulse()
			}
			writeMu.Lock()
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err = writeMessage(c, message{Type: "alive"})
			writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return sync.OnceFunc(func() {
		writeMu.Lock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := writeMessage(c, message{Type: "release"})
		writeMu.Unlock()
		if err == nil {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
			}
		}
		c.Close()
	}), nil
}

func startDaemonHeartbeat(ctx context.Context, pulse func()) (func(), error) {
	if s := accountActivity.Load(); s != nil {
		ctx, cancel := context.WithCancel(ctx)
		l := s.add(activityRequest{Kind: "heartbeat"})
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer s.remove(l)
			for {
				select {
				case <-ctx.Done():
					return
				case <-l.pulse:
					if pulse != nil {
						pulse()
					}
				}
			}
		}()
		return func() { cancel(); <-done }, nil
	}
	stop, err := leaseActivity(activityRequest{Kind: "heartbeat"}, pulse)
	if err != nil {
		return nil, err
	}
	return stop, nil
}

func activityAllowed() bool { return os.Getenv("SSHORE_DISABLED") != "1" }

func (s *daemonActivity) busy() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.leases) != 0 }

func beginCommandActivity() (func(), error) {
	if !activityAllowed() {
		return func() {}, nil
	}
	return leaseActivity(activityRequest{Kind: "heartbeat"}, nil)
}
func withDaemonActivity(execute func() (int, error)) (int, error) {
	stop, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stop()
	return execute()
}
