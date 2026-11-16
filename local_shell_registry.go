package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The token is a local connection capability, never part of public inventory.
type localShellRecord struct {
	UID                                      string `json:",omitempty"`
	Owner                                    spawnOwner
	ManagerPID                               int
	Name, Address, Token, Version, Directory string
}

func localSessionPath(name string) (string, error) {
	if err := validSession(name); err != nil {
		return "", err
	}
	dir, err := stateDir("local-shells")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

func readLocalSession(name string) (localShellRecord, error) {
	var r localShellRecord
	path, err := localSessionPath(name)
	if err != nil {
		return r, err
	}
	r, err = readLocalRecord(path, name)
	if err == nil || !os.IsNotExist(err) {
		return r, err
	}
	dir, err := stateDir("local-shells")
	if err != nil {
		return r, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidate, err := readLocalRecord(filepath.Join(dir, entry.Name()), strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return r, err
		}
		if candidate.Name == name || validTaskUID(candidate.UID) && candidate.UID[:6] == name {
			return candidate, nil
		}
	}
	return r, os.ErrNotExist
}

func readLocalRecord(path, name string) (localShellRecord, error) {
	var r localShellRecord
	if err := privateStatePath(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 16*1024)).Decode(&r)
	if err != nil {
		return r, err
	}
	host, port, err := net.SplitHostPort(r.Address)
	n, portErr := strconv.Atoi(port)
	token, tokenErr := hex.DecodeString(r.Token)
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || tokenErr != nil || len(token) != 32 || (r.Name != name && r.UID != name) || r.UID != "" && !validTaskUID(r.UID) || r.Version == "" {
		return r, fmt.Errorf("invalid local shell record: %s", name)
	}
	return r, nil
}

// Authenticate before sending any shell commands. Each exchange is bounded;
// callers clear the deadline only when they take ownership of the connection.
func dialLocalSession(r localShellRecord) (net.Conn, error) {
	return dialLocalSessionUntil(r, time.Now().Add(5*time.Second))
}

func dialLocalSessionUntil(r localShellRecord, deadline time.Time) (net.Conn, error) {
	if err := checkPeerVersion(r.Version, "local shell"); err != nil {
		return nil, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, os.ErrDeadlineExceeded
	}
	c, err := net.DialTimeout("tcp", r.Address, min(time.Second, remaining))
	if err != nil {
		return nil, fmt.Errorf("local shell %s unavailable: %w", r.Name, err)
	}
	_ = c.SetDeadline(deadline)
	if err = writeMessage(c, message{Type: "local_auth", Token: r.Token, SoftwareVersion: version}); err == nil {
		var m message
		m, err = readMessage(c)
		if err == nil {
			err = checkPeerVersion(m.SoftwareVersion, "local shell")
			if err == nil && m.Type != "local_ready" {
				err = fmt.Errorf("local shell authentication failed")
			}
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (r localShellRecord) identity() string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	return fmt.Sprintf("local:%x", sha256.Sum256([]byte(r.Token)))
}

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
	path, err := localSessionPath(r.storageName())
	if err != nil {
		return err
	}
	if err = writeStateJSON(path, r); err != nil {
		return err
	}
	s.record = r
	go watchSpawnOwner(r.Owner, s.done, s.close)
	defer func() {
		path, err := localSessionPath(s.record.storageName())
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

func (r localShellRecord) storageName() string {
	if validTaskUID(r.UID) {
		return r.UID
	}
	return r.Name
}
