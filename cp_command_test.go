package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopyParsing(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		upload bool
		host   string
		count  int
	}{
		{[]string{".:a", "work:/tmp"}, true, "work", 1},
		{[]string{".:a", ".:b", "work:/tmp"}, true, "work", 2},
		{[]string{"work:/tmp/a", ".:."}, false, "work", 1},
		{[]string{"work:/a", "work:/b", ".:./out"}, false, "work", 2},
		{[]string{".:", "user@[::1]:/tmp"}, true, "user@::1", 1},
		{[]string{"work:/a", ":"}, false, "work", 1},
		{[]string{".:./a", "work:/b", "--", "-p2222"}, true, "work", 1},
		{[]string{"self:./a", "work:/b"}, true, "work", 1},
		{[]string{"work:/a", "self:"}, false, "work", 1},
	} {
		o, err := parseCopyCommand(tc.args)
		if err != nil || o.upload != tc.upload || o.host != tc.host || len(o.sources) != tc.count {
			t.Fatal(tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"a"}, {"a", "b"}, {"a:/x", "b:/y"}, {":", ":"}, {"work:/a", "other:/b", "."}, {"a", ":", "work:/b"}, {".:a", "work:/b", "--", "-L8080:localhost:80"}, {".:a", "work:/b", "-p2222"}} {
		if _, err := parseCopyCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestLocalEndpointHints(t *testing.T) {
	for _, prefix := range []string{"self:", ".:"} {
		placeholder, err := parseCopyEndpoint(prefix)
		if err != nil || !placeholder.pick {
			t.Fatal(prefix, placeholder, err)
		}
		path, err := parseCopyEndpoint(prefix + "./downloads")
		if err != nil || path.pick || path.host != "" || path.path != "./downloads" {
			t.Fatal(prefix, path, err)
		}
	}
	kind, paths, hint, err := remoteCopyRequest([]string{"origin:/client/source", ".:/tmp"})
	if err != nil || kind != "upload" || len(paths) != 1 || hint != "/client/source" {
		t.Fatal(kind, paths, hint, err)
	}
	kind, _, hint, err = remoteCopyRequest([]string{".:report", "origin:./downloads"})
	if err != nil || kind != "download" || hint != "./downloads" {
		t.Fatal(kind, hint, err)
	}
	for _, args := range [][]string{{"origin:8080", ".:3000"}, {".:3000", "origin:8080"}, {"::8080", ".:3000"}, {".:3000", "::8080"}} {
		op, err := remoteForwardRequest(args)
		if err != nil || op.LocalHint != "8080" || !op.LocalPortPending() || op.ServerPort() != 3000 {
			t.Fatal(op, err)
		}
		if err = validateOperations([]operation{op}); err != nil {
			t.Fatal(err)
		}
		if err = validateForwardConfig(forwardConfig{Host: "work", Port: op.Port, Target: op.TargetPort}); err == nil {
			t.Fatal("hint authorized execution")
		}
		reverse := op.Reverse
		resolveLocalPort(&op, 9090)
		if op.LocalHint != "" || op.LocalPortPending() {
			t.Fatal("hint not cleared")
		}
		if reverse {
			if op.Port != 3000 || op.TargetPort != 9090 {
				t.Fatal(op)
			}
		} else {
			if op.Port != 9090 || op.TargetPort != 3000 {
				t.Fatal(op)
			}
		}
	}
	for _, args := range [][]string{{"origin:0", ".:3000"}, {".:3000", "origin:65536"}, {"origin:8080", "origin:3000"}} {
		if _, err := remoteForwardRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestCopySingleLetterSSHHost(t *testing.T) {
	endpoint, err := parseCopyEndpoint("x:/tmp/report")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.host != "x" || endpoint.path != "/tmp/report" {
		t.Fatalf("single-letter host parsed as local: %+v", endpoint)
	}
	copy, err := parseCopyCommand([]string{"x:/tmp/report", ".:./out"})
	if err != nil || copy.host != "x" || copy.upload {
		t.Fatalf("single-letter download rejected: %+v %v", copy, err)
	}
	explicit, err := parseCopyEndpoint("user@x:/tmp/report")
	if err != nil || explicit.host != "user@x" {
		t.Fatalf("explicit remote host rejected: %+v %v", explicit, err)
	}
}

func TestApprovedSFTPCancelBlockedHandshake(t *testing.T) {
	client, server := muxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		st, err := server.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = readMessage(st)
		_ = writeMessage(st, message{Type: "sftp"})
		close(ready)
		<-release
	}()
	done := make(chan error, 1)
	go func() {
		done <- executeApprovedFile(ctx, client, operation{Kind: "download", Path: "/source", Name: "source", Size: 1, LocalPath: filepath.Join(t.TempDir(), "target")}, true)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled transfer succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock SFTP")
	}
	if client.IsClosed() {
		t.Fatal("cancellation closed the attachment")
	}
}

func TestTransferProgressEstimate(t *testing.T) {
	now := time.Now()
	p := transferProgress{at: now}
	s := p.sample(now.Add(time.Second), 1024, 2048)
	if !strings.Contains(s, "remaining 1s") {
		t.Fatal(s)
	}
	if !strings.Contains(p.sample(now.Add(2*time.Second), 2048, 2048), "remaining 0s") {
		t.Fatal("completion ETA")
	}
}

func TestCopyStreamParsing(t *testing.T) {
	for _, args := range [][]string{{"-", "a", "work:/tmp/file"}, {"work:/a", "work:/b", "-"}, {"-", "-"}, {"-", ":"}} {
		if _, err := parseCopyCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, literal := range []string{".:./-", "work:-"} {
		e, err := parseCopyEndpoint(literal)
		if err != nil || e.stream {
			t.Fatal(literal, e, err)
		}
	}
	if _, _, _, err := remoteCopyRequest([]string{"-", "origin:"}); err == nil {
		t.Fatal("remote request accepted stdin")
	}
}

func TestEndpointRequests(t *testing.T) {
	for _, local := range []string{"origin:", "::"} {
		kind, paths, _, err := remoteCopyRequest([]string{local, ".:/tmp"})
		if err != nil || kind != "upload" || len(paths) != 1 || paths[0] != "/tmp" {
			t.Fatal(kind, paths, err)
		}
		kind, paths, _, err = remoteCopyRequest([]string{".:a", ".:b", local})
		if err != nil || kind != "download" || len(paths) != 2 {
			t.Fatal(kind, paths, err)
		}
		for _, reverse := range []bool{false, true} {
			args := []string{local, ".:3000"}
			if reverse {
				args = []string{".:3000", local}
			}
			op, err := remoteForwardRequest(args)
			if err != nil || !op.LocalPortPending() || op.ServerPort() != 3000 || op.Reverse != reverse {
				t.Fatal(op, err)
			}
		}
	}
	for _, args := range [][]string{{"origin:", "origin:"}, {"a", "local"}, {"a"}, {":", "a", "b"}} {
		if _, _, _, err := remoteCopyRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, args := range [][]string{{"local", "3000"}, {"3000", "8080"}, {"origin:", ":"}, {"origin:", "0"}, {"65536", ":"}} {
		if _, err := remoteForwardRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestEndpointHostValidation(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"work", "work"}, {"user@work", "user@work"}, {"[::1]", "::1"}, {"user@[::1]", "user@::1"},
	} {
		got, err := endpointHost(tc.value)
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
	for _, host := range []string{"", "-host", "host name", "host\tname", "host\x7f", "[broken]", "user@[]", "user@", "::1", "[::1"} {
		if _, err := endpointHost(host); err == nil {
			t.Fatal("accepted", host)
		}
	}
}
