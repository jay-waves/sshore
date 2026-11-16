package main

import (
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLocalShellListingBoundsUnresponsiveWorkers(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release, accepted := make(chan struct{}), make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		defer close(accepted)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() { defer connections.Done(); <-release; c.Close() }()
		}
	}()
	t.Cleanup(func() { listener.Close(); close(release); <-accepted; connections.Wait() })
	for i := range 12 {
		name := fmt.Sprintf("shell-%d", i)
		path, err := localSessionPath(name)
		if err != nil {
			t.Fatal(err)
		}
		record := localShellRecord{Name: name, Address: listener.Addr().String(), Token: resourceToken(), Version: version}
		if err := writeStateJSON(path, record); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	rows, err := localShells()
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("status queries took %s", elapsed)
	}
	if len(rows) != 12 {
		t.Fatalf("returned %d rows", len(rows))
	}
	for _, row := range rows {
		if row.State != "unavailable" {
			t.Fatalf("unresponsive shell state: %s", row.State)
		}
	}
}
