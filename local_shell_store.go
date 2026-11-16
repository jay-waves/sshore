package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The token is a local connection capability, never part of public inventory.
type localShellRecord struct {
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
	return readLocalRecord(path, name)
}

func readLocalRecord(path, name string) (localShellRecord, error) {
	var r localShellRecord
	if err := privateState(path, false); err != nil {
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
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || tokenErr != nil || len(token) != 32 || r.Name != name || r.Version == "" {
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
	return fmt.Sprintf("local:%x", sha256.Sum256([]byte(r.Token)))
}
