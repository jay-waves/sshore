package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The listener owns no terminal until the first authenticated attachment.
// serve runs synchronously for each connection; shutdown joins all handlers.
func serveLocalShell(s *localShell, r localShellRecord, ready func(localShellRecord) error, serve func(net.Conn, message) error, done <-chan struct{}) (result error) {
	if err := validSession(r.Name); err != nil {
		return err
	}
	if r.Version != version || len(r.Token) != 64 || !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local startup request")
	}
	owner, err := lockObjectName(r.Name)
	if err != nil {
		return err
	}
	s.nameLock = owner
	defer func() { s.nameLock.Close() }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	r.Address = listener.Addr().String()
	path, err := localSessionPath(r.Name)
	if err != nil {
		return err
	}
	if err = writeStateJSON(path, r); err != nil {
		return err
	}
	s.record = r
	go watchSpawnOwner(r.Owner, s.done, s.close)
	defer func() {
		path, err := localSessionPath(s.record.Name)
		if err == nil {
			os.Remove(path)
		}
	}()
	if err = ready(r); err != nil {
		return err
	}
	startupTimer := time.AfterFunc(15*time.Second, func() {
		s.mu.Lock()
		empty := s.terminal == nil
		if empty {
			s.lifecycle.End()
		}
		s.mu.Unlock()
		if empty {
			s.close()
		}
	})
	defer startupTimer.Stop()
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-done:
			_ = listener.Close()
		case <-stopped:
		}
	}()
	defer func() {
		mu.Lock()
		for c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		workers.Wait()
	}()
	for {
		c, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
				return nil
			default:
				return err
			}
		}
		mu.Lock()
		if len(connections) >= 32 {
			mu.Unlock()
			c.Close()
			continue
		}
		connections[c] = true
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { c.Close(); mu.Lock(); delete(connections, c); mu.Unlock() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			m, err := readMessage(c)
			if err != nil || m.Type != "local_auth" || subtle.ConstantTimeCompare([]byte(m.Token), []byte(r.Token)) != 1 {
				return
			}
			if m.SoftwareVersion != version {
				_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "local shell version mismatch"})
				return
			}
			if writeMessage(c, message{Type: "local_ready", SoftwareVersion: version}) != nil {
				return
			}
			m, err = readMessage(c)
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Time{})
			_ = serve(c, m)
		}()
	}
}
