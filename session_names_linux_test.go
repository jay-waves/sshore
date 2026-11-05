package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionReservationReusesDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "ss-names-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	lock, conn, err := reserveSession("main")
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("unexpected existing session")
	}
	lock.Close()
	listener, err := net.Listen("unix", filepath.Join(dir, "main.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lock, conn, err = reserveSession("main")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if conn == nil {
		t.Fatal("did not reuse existing daemon")
	}
	conn.Close()
}
