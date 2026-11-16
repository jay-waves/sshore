package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

func TestLocalShellEnvironment(t *testing.T) {
	source := []string{"SSHORE_SOCKET=remote", "SSH_CONNECTION=old", "SSHORE_LOCAL_SESSION=old", "TERM=old", "sshore_socket=mixed", "Term=mixed", "KEEP=value"}
	got := localShellEnvironment(source, "xterm", "local:new")
	for _, old := range source[:4] {
		if slices.Contains(got, old) {
			t.Fatalf("retained %s", old)
		}
	}
	for _, mixed := range source[4:6] {
		if slices.Contains(got, mixed) == (runtime.GOOS == "windows") {
			t.Fatalf("incorrect case handling: %s", mixed)
		}
	}
	for _, want := range []string{"KEEP=value", "TERM=xterm", "SSHORE_LOCAL_SESSION=local:new"} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if source[0] != "SSHORE_SOCKET=remote" {
		t.Fatal("mutated source environment")
	}
}

func TestSpawnOptionsAndInventory(t *testing.T) {
	for _, args := range [][]string{{"spawn"}, {"spawn", "--local", "dev", "--detach"}, {"spawn", "host", "dev", "--", "-p", "2222"}} {
		o, err := parseOptions(args)
		if err != nil || o.command != "spawn" {
			t.Fatalf("%v: %+v %v", args, o, err)
		}
	}
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{LocalShells: []localShellView{{ID: 1, Name: "persistent", State: "detached"}, {ID: 2, Name: "temporary", State: "detached", Temporary: true}}, Shells: []shellView{{TaskID: 3, Name: "remote", Host: "host", State: "detached", Temporary: true, Historical: true}}})
	for _, want := range []string{"(persistent)", "(temporary)", "(remote)", "launch", "spawn", "never checked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "MODE") {
		t.Fatal("unexpected extra mode column")
	}
}

func TestClientConnectionCancellation(t *testing.T) {
	for _, stage := range []string{"authentication", "nested-route"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			path, err := localSessionPath("root")
			if err != nil {
				t.Fatal(err)
			}
			record := localShellRecord{Name: "root", Address: listener.Addr().String(), Token: resourceToken(), Version: version}
			if err = writeStateJSON(path, record); err != nil {
				t.Fatal(err)
			}
			reached, closed := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(closed)
				c, err := listener.Accept()
				if err != nil {
					reached <- err
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				_, err = readMessage(c)
				if err == nil && stage == "nested-route" {
					err = writeMessage(c, message{Type: "local_ready", SoftwareVersion: version})
					if err == nil {
						_, err = readMessage(c)
					}
					if err == nil {
						err = writeMessage(c, message{Type: "route_connected", SoftwareVersion: version})
					}
					if err == nil {
						_, err = readMessage(c)
					}
				}
				reached <- err
				_, _ = io.Copy(io.Discard, c)
			}()
			root := options{command: "attach", local: true, session: "root"}
			o := root
			if stage == "nested-route" {
				parent := options{command: "attach", routeParent: &root, routeToken: "first"}
				o = options{command: "attach", routeParent: &parent, routeToken: "second"}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, closeTransport, err := openClientTransportContext(ctx, &o)
				if closeTransport != nil {
					closeTransport()
				}
				result <- err
			}()
			select {
			case err := <-reached:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("connection did not reach handshake")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled connection remained blocked")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("cancelled connection remained open")
			}
		})
	}
}
