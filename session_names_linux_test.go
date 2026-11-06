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
	// Occupied names include detached daemons: allocation does not depend on
	// whether a terminal client is attached to this listening socket.
	if _, _, _, err := reserveSession([]string{"pikachu"}, true); err == nil || !strings.Contains(err.Error(), "all automatic session names") {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	name, lock, conn, err := reserveSession([]string{"pikachu", "eevee"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if name != "eevee" || conn != nil {
		t.Fatalf("allocated %q, connection %v", name, conn)
	}
	// Explicit names may still connect to an existing daemon.
	name, explicitLock, conn, err := reserveSession([]string{"pikachu"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer explicitLock.Close()
	defer conn.Close()
	if name != "pikachu" {
		t.Fatal(name)
	}
	listener.Close()
	// A leftover startup lock does not permanently consume the name.
	explicitLock.Close()
	name, reusedLock, reusedConn, err := reserveSession([]string{"pikachu"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reusedLock.Close()
	if name != "pikachu" || reusedConn != nil {
		t.Fatalf("reuse: %q %v", name, reusedConn)
	}
}
