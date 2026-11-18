package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestApprovedForwardCarriesSourceInstance(t *testing.T) {
	instance := strings.Repeat("c", 32)
	command := approvedForwardCommand(forwardConfig{Session: "work", ResumePID: 123, ApprovalID: strings.Repeat("a", 32), Instance: instance})
	if !strings.HasSuffix(command, " "+shellQuote(instance)) {
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
		c := forwardConfig{Host: "work", Port: 8080, Target: 3000, Reverse: reverse, SSH: []string{"-p", "2222", "-L", "9999:localhost:9"}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		options := forwardSSHOptions(c, "opaque helper")
		if options.Host != c.Host || options.Command != "opaque helper" || options.Forward.Port != 8080 || options.Forward.Target != 3000 || options.Forward.Reverse != reverse || strings.Contains(strings.Join(options.Args, " "), "9999") {
			t.Fatal(options)
		}
	}
	for _, c := range []forwardConfig{{Port: 0, Target: 80}, {Port: 80, Target: 65536}, {Port: 80, Target: 80, Retries: 11}} {
		if c.Validate() == nil {
			t.Fatal("invalid config accepted", c)
		}
	}
}

func TestForwardTransportDoesNotReplayOtherRules(t *testing.T) {
	options := forwardSSHOptions(forwardConfig{Host: "work", Port: 8080, Target: 3000, SSH: []string{"-p", "2222", "-L", "9999:localhost:9999", "-R", "8888:localhost:8888", "-D", "7777", "-o", "RemoteForward=6666 localhost:66", "-o", "LocalForward 5555 localhost:55"}}, "opaque helper")
	joined := strings.Join(options.Args, " ")
	if strings.Contains(joined, "9999") || strings.Contains(joined, "8888") || strings.Contains(joined, "7777") || strings.Contains(joined, "6666") || strings.Contains(joined, "5555") || !strings.Contains(joined, "-p 2222") {
		t.Fatal(options)
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

func TestForwardTransportOptions(t *testing.T) {
	f, fg, e := parseForwardCommand([]string{".:8080", "work:3000", "--", "-Jjump", "-i", "key with spaces", "-p2222", "-F", "config"})
	if e != nil || fg || f.Name != "" || strings.Join(f.SSH, "|") != "-J|jump|-i|key with spaces|-p|2222|-F|config" {
		t.Fatal(f, fg, e)
	}
	command := strings.Join(forwardSSHOptions(f, forwardRemoteCommand(f)).Args, " ")
	if !strings.Contains(command, "-J jump") {
		t.Fatal(command)
	}
	for _, bad := range [][]string{{".:8080", "work:3000", "-i"}, {".:8080", "work:3000", "-D", "1080"}, {".:8080", "work:3000", "--unknown"}} {
		if _, _, e := parseForwardCommand(bad); e == nil {
			t.Fatal(bad)
		}
	}
}

func TestBlockedHeartbeatCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runForwardTransport(ctx, forwardTransport{
			Log: io.Discard,
			Start: func(ctx context.Context, _ bool, _ io.Writer) (*forwardConnection, error) {
				client, server := net.Pipe()
				go func() { io.WriteString(server, forwardReadyMarker+"\n"); <-ctx.Done(); server.Close() }()
				stop := context.AfterFunc(ctx, func() { client.Close() })
				return &forwardConnection{Input: client, Output: client, Close: func() error { stop(); return client.Close() }, Wait: func() error { <-ctx.Done(); return ctx.Err() }}, nil
			},
			Ready: func() error { return nil }, Status: func(string, error) {},
			Heartbeat: func() ([]byte, error) { close(heartbeat); return bytes.Repeat([]byte("x"), 1<<20), nil },
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
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not join blocked heartbeat and process")
	}
}

func TestForwardInvocation(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		reverse      bool
		port, target int
	}{
		{[]string{".:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", ".:8080"}, true, 3000, 8080},
		{[]string{"self:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", "self:8080"}, true, 3000, 8080},
		{[]string{"self:", "work:3000"}, false, 0, 3000},
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
	for _, args := range [][]string{{"origin:work:3000", "8080"}, {"origin:0", "work:3000"}, {"work:3000", "origin:65536"}, {"origin:3000", ".:8080"}, {"origin:3000", "self:8080"}, {"self:8080", "work:"}, {"3000", "8080"}} {
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
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("peer refused input")
		started := false
		code, err := runAttempt(context.Background(), forwardTransport{
			Log: io.Discard,
			Start: func(ctx context.Context, _ bool, _ io.Writer) (*forwardConnection, error) {
				return &forwardConnection{
					Input:  failedHeartbeatInput{failure},
					Output: strings.NewReader(forwardReadyMarker + "\n"),
					Close:  func() error { return nil },
					Wait:   func() error { <-ctx.Done(); return ctx.Err() },
				}, nil
			},
			Ready:     func() error { return nil },
			Heartbeat: func() ([]byte, error) { return []byte("heartbeat"), nil },
		}, true, &started)
		if code != 1 || !errors.Is(err, failure) || !strings.Contains(err.Error(), "heartbeat write failed") {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
}
