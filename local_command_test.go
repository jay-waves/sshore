package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHostedShellHandoffPreservesLocalInstance(t *testing.T) {
	uid := strings.Repeat("a", 32)
	h := clientHandoff{Hosted: true, Args: []string{"attach", "here:work"}, Directory: t.TempDir(), TaskIdentity: "task:" + uid, Instance: uid}
	o, err := h.options()
	if err != nil || !o.local || !o.hosted || o.instance != uid || o.taskIdentity != h.TaskIdentity {
		t.Fatalf("hosted route lost: %+v %v", o, err)
	}
	h.Args = []string{"attach", "remote:work"}
	if _, err := h.options(); err == nil {
		t.Fatal("hosted route accepted an SSH destination")
	}
}

func TestNativeShellListingReadsDaemonState(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	release := make(chan struct{})
	defer close(release)
	for i := range 12 {
		r := registerTestShell(t, tasks, fmt.Sprintf("shell-%d", i), fmt.Sprintf("%032x", i+1))
		tasks.records[r.UID].(*nativeShellTask).serve = func(c net.Conn, m message) error { <-release; return nil }
	}
	start := time.Now()
	rows, err := nativeShells()
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
		if row.State != "starting" {
			t.Fatalf("unexpected registered shell state: %s", row.State)
		}
	}
}

func TestLocalShellEnvironment(t *testing.T) {
	source := []string{"SSHORE_SOCKET=remote", "SSH_CONNECTION=old", "SSHORE_LOCAL_SESSION=old", "TERM=old", "SSHORE_TTY=/dev/stale", "sshore_socket=mixed", "Term=mixed", "KEEP=value"}
	got := localShellEnvironment(source, "xterm", "local:new")
	for _, old := range source[:5] {
		if slices.Contains(got, old) {
			t.Fatalf("retained %s", old)
		}
	}
	for _, mixed := range source[5:7] {
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

func TestHostedShellEnvironment(t *testing.T) {
	source := []string{"PATH=" + filepath.Join("base", "bin"), "KEEP=value"}
	executable := filepath.Join("install", "sshore")
	got := hostedShellEnvironment(source, "xterm", "instance", executable)
	for _, want := range []string{
		"TERM=xterm", "SSHORE_SOCKET=instance", "SSHORE_TREE_ID=" + remoteTreeIdentity("instance"),
		"PATH=" + filepath.Dir(executable) + string(os.PathListSeparator) + filepath.Join("base", "bin"),
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if len(source) != 2 || source[0] != "PATH="+filepath.Join("base", "bin") {
		t.Fatal("mutated source environment")
	}
}

func TestSpawnOptionsAndInventory(t *testing.T) {
	for _, args := range [][]string{{"spawn"}, {"spawn", "here:", "--detach"}, {"spawn", "host:", "--", "-p", "2222"}} {
		o, err := parseOptions(args)
		if err != nil || o.command != "spawn" {
			t.Fatalf("%v: %+v %v", args, o, err)
		}
	}
	var out bytes.Buffer
	writeTaskInventory(&out, inventory{LocalShells: []localShellView{{Name: "persistent", State: "detached"}, {Name: "temporary", State: "detached", Temporary: true}}, Shells: []shellView{{Name: "remote", Host: "host", State: "detached", Temporary: true, Historical: true}}})
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
			tasks := startRequestDaemon(t)
			record := registerTestShell(t, tasks, "root", strings.Repeat("a", 32))
			daemon, err := existingDaemonRecord()
			if err != nil {
				t.Fatal(err)
			}
			path, err := daemonPath()
			if err != nil {
				t.Fatal(err)
			}
			daemon.Address = listener.Addr().String()
			if err := writeStateJSON(path, daemon); err != nil {
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
				_, err = http.ReadRequest(bufio.NewReader(c))
				if err == nil && stage == "nested-route" {
					_, err = fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\nSshore-Version: %s\r\n\r\n", daemonUpgrade, version)
					if err == nil {
						err = json.NewEncoder(c).Encode(localStartup{Record: record})
					}
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
				c, err := openClientTransportContext(ctx, &o, nil)
				if c != nil {
					c.Close()
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

func TestShellStartupOptions(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		for _, flag := range []bool{false, true} {
			args := []string{command, "host:"}
			if flag {
				args = append(args, "--no-login")
			}
			o, err := parseOptions(args)
			if err != nil || o.noLogin != flag {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
		}
	}
	for _, args := range [][]string{{"attach", "host", "--no-login"}, {"launch", "host", "--login"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := validateAttach(message{Type: "attach", Term: "xterm", Action: "existing", NoLogin: true}); err == nil {
		t.Fatal("accepted startup option on reattach")
	}
}

func TestNoProfileOptions(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		o, err := parseOptions([]string{command, "host:", "--no-profile", "--no-login"})
		if err != nil || !o.noProfile || !o.noLogin {
			t.Fatalf("%+v %v", o, err)
		}
	}
	if _, err := parseOptions([]string{"attach", "host", "--no-profile"}); err == nil {
		t.Fatal("accepted creation flag on attach")
	}
	if err := validateAttach(message{Type: "attach", Term: "xterm", Action: "existing", NoProfile: true}); err == nil {
		t.Fatal("accepted profile flag on reattach")
	}
}

func TestLocalCommandRouting(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		local bool
		name  string
	}{
		{[]string{"launch"}, true, ""},
		{[]string{"launch", "--quiet", "--no-escape"}, true, ""},
		{[]string{"launch", "here:"}, true, ""},
		{[]string{"launch", "work:"}, false, ""},
		{[]string{"attach"}, false, ""},
		{[]string{"list", "here:"}, true, ""},
	} {
		o, err := parseOptions(tc.args)
		local := o.local
		if err != nil || local != tc.local || o.session != tc.name {
			t.Fatalf("%v: %+v %v %v", tc.args, o, local, err)
		}
	}
}

func TestLocalCommandRejectsAmbiguousOptions(t *testing.T) {
	for _, args := range [][]string{
		{"launch", "host", "--name", "work"},
		{"launch", "--name="},
		{"launch", "--local", "--server", "other"},
		{"attach", "--local", "../work"},
		{"kill", "--local"},
		{"rename", "--local", "work"},
		{"list", "--local", "--json", "--verbose"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
