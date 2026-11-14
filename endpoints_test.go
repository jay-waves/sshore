package main

import "testing"

func TestEndpointRequests(t *testing.T) {
	for _, local := range []string{"origin:", ":"} {
		kind, paths, _, err := remoteCopyRequest([]string{local, "/tmp"})
		if err != nil || kind != "upload" || len(paths) != 1 || paths[0] != "/tmp" {
			t.Fatal(kind, paths, err)
		}
		kind, paths, _, err = remoteCopyRequest([]string{"a", "b", local})
		if err != nil || kind != "download" || len(paths) != 2 {
			t.Fatal(kind, paths, err)
		}
		for _, reverse := range []bool{false, true} {
			args := []string{local, "3000"}
			if reverse {
				args = []string{"3000", local}
			}
			op, err := remoteForwardRequest(args)
			if err != nil || !op.localPortPending() || op.serverPort() != 3000 || op.Reverse != reverse {
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

func TestForwardInvocation(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		reverse      bool
		port, target int
	}{
		{[]string{"origin:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", "origin:8080"}, true, 3000, 8080},
		{[]string{"8080", "work:3000"}, false, 8080, 3000},
	} {
		f, _, err := parseForwardCommand(tc.args)
		if err != nil || f.Host != "work" || f.Port != tc.port || f.Target != tc.target || f.Reverse != tc.reverse {
			t.Fatal(f, err)
		}
	}
}

func TestForwardRejectsInvalidLocalEndpoint(t *testing.T) {
	for _, args := range [][]string{{"origin:work:3000", "8080"}, {"origin:0", "work:3000"}, {"work:3000", "origin:65536"}, {"origin:3000", "origin:8080"}, {"3000", "8080"}} {
		if _, _, err := parseForwardCommand(args); err == nil {
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
