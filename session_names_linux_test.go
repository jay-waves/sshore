package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutomaticSessionReservation(t *testing.T) {
	dir, err := os.MkdirTemp("", "ss-names-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "pikachu.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lifetime, err := openLock(filepath.Join(dir, "pikachu.sock.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer lifetime.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if m, err := readMessage(c); err == nil && m.Type == "status" {
					_ = writeMessage(c, message{Type: "session", Name: "pikachu"})
				}
			}()
		}
	}()
	// Occupied names include detached daemons: allocation does not depend on
	// whether a terminal client is attached to this listening socket.
	if _, err := reserveSession([]string{"pikachu"}, true); err == nil || !strings.Contains(err.Error(), "all automatic session names") {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	r, err := reserveSession([]string{"pikachu", "eevee"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.startup.Close()
	if r.name != "eevee" || r.conn != nil {
		t.Fatalf("allocated %q, connection %v", r.name, r.conn)
	}
	// Explicit names may still connect to an existing daemon.
	explicit, err := reserveSession([]string{"pikachu"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.startup.Close()
	defer explicit.conn.Close()
	if explicit.name != "pikachu" {
		t.Fatal(explicit.name)
	}
	listener.Close()
	lifetime.Close()
	// A leftover startup lock does not permanently consume the name.
	explicit.startup.Close()
	reused, err := reserveSession([]string{"pikachu"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reused.startup.Close()
	if reused.name != "pikachu" || reused.conn != nil {
		t.Fatalf("reuse: %q %v", reused.name, reused.conn)
	}
}

// Neither a hung daemon nor stale metadata should turn name lookup into a
// network health check. A live hung daemon must still reserve its public name.
func TestSessionNameResolutionWithoutStatus(t *testing.T) {
	dir, err := os.MkdirTemp("", "ss-label-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	path := filepath.Join(dir, "internal.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() // Deliberately never accepts or answers status.
	lock, err := openLock(path+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeSessionName(path, "work"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSession("work"); err != nil || got != path {
		t.Fatalf("%q: %v", got, err)
	}
	if got, err := resolveSession("other"); err != nil || got != "" {
		t.Fatalf("%q: %v", got, err)
	}
	if _, err := reserveSession([]string{"work"}, true); err == nil {
		t.Fatal("reallocated live label")
	}
	// Losing the socket must not release a still-running daemon's label.
	listener.Close()
	if got, err := resolveSession("work"); err != nil || got != path {
		t.Fatalf("live identity without socket: %q: %v", got, err)
	}
	if _, err := reserveSession([]string{"work"}, false); err == nil {
		t.Fatal("unavailable live identity treated as a new session")
	}
	lock.Close()
	if got, err := resolveSession("work"); err != nil || got != "" {
		t.Fatalf("stale label: %q: %v", got, err)
	}
}
