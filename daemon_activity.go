package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const daemonHeartbeatInterval = 20 * time.Second
const daemonHeartbeatTimeout = 75 * time.Second

type leaseRequest struct {
	Kind string
	Peer *peerRecord
}
type activityLease struct {
	order uint64
	peer  *peerRecord
}

// All live advertisements and leases belong to the account daemon. No state
// file or PID scan can recreate a lease after its connection has gone away.
type daemonActivity struct {
	hostInfo      atomic.Pointer[hostInfo]
	root          string
	instance      string
	inventoryMu   sync.Mutex
	published     []byte
	tasks         *daemonTasks
	mu            sync.Mutex
	leases        map[*activityLease]struct{}
	nextPeerOrder uint64
}

var accountActivity atomic.Pointer[daemonActivity]

func (s *daemonActivity) registerLease(request leaseRequest) (*activityLease, error) {
	if request.Kind != "incoming" && request.Peer != nil {
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
	default:
		return nil, fmt.Errorf("invalid activity lease kind")
	}
	s.mu.Lock()
	if s.leases == nil {
		s.leases = make(map[*activityLease]struct{})
	}
	if l.peer != nil {
		s.nextPeerOrder++
		l.order = s.nextPeerOrder
	}
	s.leases[l] = struct{}{}
	s.mu.Unlock()
	if l.peer != nil {
		s.publishInventory()
	}
	return l, nil
}
func (s *daemonActivity) remove(l *activityLease) {
	s.mu.Lock()
	delete(s.leases, l)
	s.mu.Unlock()
	if l.peer != nil {
		s.publishInventory()
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
	record, err := ensureLocalDaemonContext(ctx)
	if err != nil {
		return nil, err
	}
	return dialDaemonStream(ctx, record, time.Now().Add(10*time.Second), "/leases", r)
}
func (s *daemonActivity) serve(c net.Conn, r leaseRequest, heartbeat bool) error {
	l, err := s.registerLease(r)
	if err != nil {
		return err
	}
	if err := writeMessage(c, message{Type: "activity_ready"}); err != nil {
		s.remove(l)
		_ = c.Close()
		return err
	}
	_ = c.SetDeadline(time.Time{})
	if !heartbeat {
		// Both endpoints suspend together. Only closure ends a native shell's lease.
		defer s.remove(l)
		defer c.Close()
		_, err := readMessage(c)
		if err == nil {
			return fmt.Errorf("unexpected native owner message")
		}
		return err
	}
	pulse := time.NewTicker(daemonHeartbeatInterval)
	defer pulse.Stop()
	done := make(chan error, 1)
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
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = fmt.Errorf("heartbeat acknowledgement timed out: %w", err)
				}
				done <- err
				return
			}
			switch m.Type {
			case "alive":
			case "release":
				return
			default:
				done <- fmt.Errorf("unexpected heartbeat reply")
				return
			}
		}
	}()
	for {
		select {
		case err := <-done:
			return err
		case <-pulse.C:
			if err := sendLeaseMessage(c, "pulse"); err != nil {
				return fmt.Errorf("send heartbeat: %w", err)
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
	return acceptActivityLease(ctx, c, true)
}

func acceptActivityLease(ctx context.Context, c net.Conn, heartbeat bool) (func(), <-chan struct{}, error) {
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
	finished := make(chan struct{})
	if !heartbeat {
		go func() {
			defer close(finished)
			defer closeConnection()
			defer stop()
			_, _ = readMessage(c)
		}()
		return func() { closeConnection(); <-finished }, finished, nil
	}
	var writeMu sync.Mutex
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

func (s *daemonActivity) busy() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.leases) != 0 }

func beginCommandActivity() (func(), error) {
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
