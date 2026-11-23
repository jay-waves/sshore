package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

func TestApprovedForwardCarriesSourceInstance(t *testing.T) {
	instance := strings.Repeat("c", 32)
	command := forwardRemoteCommand(forwardConfig{Session: "work", ResumePID: 123, ApprovalID: strings.Repeat("a", 32), Instance: instance})
	if !strings.Contains(command, `"instance":"`+instance+`"`) {
		t.Fatalf("source instance missing from forward validation: %s", command)
	}
}

func TestForwardArguments(t *testing.T) {
	o, err := parseOptions([]string{"launch", "host:", "--", "-L", "127.0.0.1:8080:localhost:80"})
	if err != nil || len(o.ssh) != 2 {
		t.Fatal(o, err)
	}
	connection := sshConnectionOptions(o)
	if connection.Args[0] != "-L" || connection.Args[1] != o.ssh[1] {
		t.Fatal(connection.Args)
	}
}

func TestForwardNativeOptions(t *testing.T) {
	o, err := parseOptions([]string{"launch", "host:", "--", "-L8080:localhost:80", "-L", "8081:localhost:81"})
	if err != nil || len(o.ssh) != 4 || o.ssh[1] != "8080:localhost:80" || o.ssh[3] != "8081:localhost:81" {
		t.Fatal(o, err)
	}
}

func TestLocalForwardDirections(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		host    string
		reverse bool
	}{
		{[]string{".:8080", "work:3000"}, "work", false},
		{[]string{"work:8080", ".:3000"}, "work", true},
		{[]string{".:8080", "user@work:3000"}, "user@work", false},
		{[]string{"[::1]:8080", ".:3000"}, "::1", true},
		{[]string{".:8080", "user@[::1]:3000"}, "user@::1", false},
	} {
		f, _, err := parseForwardCommand(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if err != nil || f.Host != tc.host || f.Port != 8080 || f.Target != 3000 || f.Reverse != tc.reverse {
			t.Fatal(f, err)
		}
	}
	for _, args := range [][]string{
		{"work", "3000"}, {"work", "8080", "3000"}, {"3000", "from", "work:8080"},
		{".:8080", "3000"}, {"a:8080", "b:3000"},
		{".:8080", ":3000"}, {".:8080", "-host:3000"},
		{".:8080", "work:0"}, {"work:65536", "80"},
		{".:8080", "::1:3000"}, {".:8080", "[bad]:3000"},
		{".:8080", "work:3000", "extra"},
	} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestForwardDiagnosticsBounded(t *testing.T) {
	var d diagnosticsTail
	d.Write([]byte(strings.Repeat("x", 8192)))
	d.Write([]byte("\nError: remote port forwarding failed for listen port 5173\n"))
	if len(d.data) > 4096 || !strings.Contains(d.String(), "listen port 5173") {
		t.Fatalf("diagnostic tail lost: %q", d.String())
	}
}

func TestTransportArgumentsAndConfig(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		c := forwardConfig{Host: "work", Port: 8080, Target: 3000, Reverse: reverse,
			SSH: []string{"-p", "2222", "-L", "9999:localhost:9", "-o", "ServerAliveInterval=0"}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		options := forwardSSHOptions(c, "opaque helper")
		if options.Host != c.Host || options.Command != "opaque helper" || strings.Join(options.Args, " ") != strings.Join(c.SSH, " ") {
			t.Fatal(options)
		}
	}
	for _, c := range []forwardConfig{{Port: 0, Target: 80}, {Port: 80, Target: 65536}, {Port: 80, Target: 80, Retries: 11}} {
		if c.Validate() == nil {
			t.Fatal("invalid config accepted", c)
		}
	}
}

func TestRetryPolicy(t *testing.T) {
	for _, value := range []string{"0", "1", "10"} {
		n, err := parseForwardRetries(value)
		if err != nil || (forwardConfig{Retries: n, Port: 80, Target: 80}).Validate() != nil {
			t.Fatalf("%q: %d %v", value, n, err)
		}
	}
	for _, value := range []string{"", "-1", "11", "invalid", "9999999999999999999999"} {
		if _, err := parseForwardRetries(value); err != errForwardRetries {
			t.Fatalf("%q: %v", value, err)
		}
	}
}

func TestForwardRetryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		established   bool
		cancelBackoff bool
		wantAttempts  int
	}{
		{"initial-failure", false, false, 1},
		{"exhausted", true, false, 2},
		{"cancel-backoff", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s := &resourceService{log: io.Discard}
				failure := errors.New("transport lost")
				var attempts []bool
				type result struct {
					code int
					err  error
				}
				done := make(chan result, 1)
				go func() {
					code, err := retryForward(ctx, 1, s, func(initial bool) (bool, error) {
						attempts = append(attempts, initial)
						return tc.established, failure
					})
					done <- result{code, err}
				}()
				if tc.cancelBackoff {
					synctest.Wait()
					if s.statusSnapshot().Transport != "reconnecting" {
						t.Fatal("missing reconnecting state")
					}
					cancel()
				}
				got := <-done
				if tc.cancelBackoff && (got.code != 0 || got.err != nil) || !tc.cancelBackoff && (got.code != 1 || !errors.Is(got.err, failure)) {
					t.Fatalf("result: %+v", got)
				}
				if len(attempts) != tc.wantAttempts || !attempts[0] || len(attempts) > 1 && attempts[1] {
					t.Fatalf("attempts: %v", attempts)
				}
			})
		})
	}
}

func TestForwardTransportOptions(t *testing.T) {
	f, fg, e := parseForwardCommand([]string{".:8080", "work:3000", "--", "-Jjump", "-i", "key with spaces", "-p2222", "-F", "config"})
	if e != nil || fg || f.Name != "" || strings.Join(f.SSH, "|") != "-J|jump|-i|key with spaces|-p|2222|-F|config" {
		t.Fatal(f, fg, e)
	}
	command := strings.Join(forwardSSHOptions(f, forwardRemoteCommand(f)).Args, " ")
	if !strings.Contains(command, "-J jump") {
		t.Fatal(command)
	}
	for _, bad := range [][]string{{".:8080", "work:3000", "-i"}, {".:8080", "work:3000", "-D"}, {".:8080", "work:3000", "--unknown"}} {
		if _, _, e := parseForwardCommand(bad); e == nil {
			t.Fatal(bad)
		}
	}
}

func TestBlockedHeartbeatCancellation(t *testing.T) {
	activityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runForwardAttempt(ctx, &resourceService{log: io.Discard}, []byte("heartbeat"), nil,
			func(ctx context.Context, _ io.Writer) (*forwardConnection, error) {
				client, server := net.Pipe()
				go func() { io.WriteString(server, forwardReadyMarker+"\n"); <-ctx.Done(); server.Close() }()
				return &forwardConnection{
					Writer: observedHeartbeatInput{Writer: client, started: heartbeat},
					Reader: client,
					close:  client.Close,
					Wait:   func() error { <-ctx.Done(); return ctx.Err() },
				}, nil
			})
		done <- err
	}()
	select {
	case <-heartbeat:
	case err := <-done:
		t.Fatalf("exited before heartbeat: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup stalled")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not join blocked heartbeat and process")
	}
}

type observedHeartbeatInput struct {
	io.Writer
	started chan struct{}
}

func (w observedHeartbeatInput) Write(p []byte) (int, error) {
	close(w.started)
	return w.Writer.Write(p)
}

func TestForwardInvocation(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		reverse      bool
		port, target int
	}{
		{[]string{".:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", ".:8080"}, true, 3000, 8080},
		{[]string{"here:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", "here:8080"}, true, 3000, 8080},
		{[]string{"here:", "work:3000"}, false, 0, 3000},
		{[]string{"work:3000", ".:"}, true, 3000, 0},
		{[]string{".:8080", "work:3000"}, false, 8080, 3000},
	} {
		f, _, err := parseForwardCommand(tc.args)
		if err != nil || f.Host != "work" || f.Port != tc.port || f.Target != tc.target || f.Reverse != tc.reverse {
			t.Fatal(f, err)
		}
	}
}

func TestForwardRejectsInvalidLocalEndpoint(t *testing.T) {
	for _, args := range [][]string{{"origin:work:3000", "8080"}, {"origin:0", "work:3000"}, {"work:3000", "origin:65536"}, {"origin:3000", ".:8080"}, {"origin:3000", "here:8080"}, {"here:8080", "work:"}, {"3000", "8080"}} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// A failed heartbeat must end the attempt with its original cause, even when
// the process waiter concurrently observes the resulting cancellation.
type failedHeartbeatInput struct{ err error }

func (w failedHeartbeatInput) Write([]byte) (int, error) { return 0, w.err }
func (w failedHeartbeatInput) Close() error              { return nil }

func TestForwardHeartbeatPreservesExitCause(t *testing.T) {
	activityFixture(t)
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("peer refused input")
		ready, err := runForwardAttempt(context.Background(), &resourceService{log: io.Discard}, []byte("heartbeat"), nil,
			func(ctx context.Context, _ io.Writer) (*forwardConnection, error) {
				return &forwardConnection{
					Writer: failedHeartbeatInput{failure},
					Reader: strings.NewReader(forwardReadyMarker + "\n"),
					close:  func() error { return nil },
					Wait:   func() error { <-ctx.Done(); return ctx.Err() },
				}, nil
			})
		if !ready || !errors.Is(err, failure) || !strings.Contains(err.Error(), "heartbeat write failed") {
			t.Fatalf("ready=%v err=%v", ready, err)
		}
	})
}

func TestDirectForwardRejectsInvalidRuleBeforeDial(t *testing.T) {
	for _, rule := range []forwardConfig{{Port: 0, Target: 80}, {Port: 80, Target: 65536}} {
		client, close, err := openDirectForward(context.Background(), sshio.Options{}, rule)
		if err == nil || err.Error() != "invalid forward ports" || client != nil || close != nil {
			t.Fatalf("invalid rule reached SSH: client=%v error=%v", client, err)
		}
	}
}

func TestDirectForwardListenerOwnership(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	rule := forwardConfig{Port: occupied.Addr().(*net.TCPAddr).Port, Target: 80}
	// Binding fails before any SSH channel or remote helper is opened.
	if worker, err := startDirectForward(nil, rule); err == nil {
		worker.Close()
		t.Fatal("occupied listener accepted")
	}
	occupied.Close()
	worker, err := startDirectForward(nil, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.Close()
	// Closing the forward releases its listener without closing another owner.
	reopened, err := net.Listen("tcp4", occupied.Addr().String())
	if err != nil {
		t.Fatal("forward retained listener:", err)
	}
	reopened.Close()
}
