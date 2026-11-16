package main

import (
	"testing"
)

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

func TestSSHSeparatorBoundaries(t *testing.T) {
	parsers := map[string]func([]string) error{
		"session": func(a []string) error { _, e := parseOptions(append([]string{"launch"}, a...)); return e },
		"mount":   func(a []string) error { _, e := parseFTPOptions(a); return e },
		"forward": func(a []string) error {
			args := append([]string{"8080", a[0] + ":3000"}, a[1:]...)
			_, _, e := parseForwardCommand(args)
			return e
		},
	}
	for name, parse := range parsers {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{{"work"}, {"work", "--", "-Jjump", "-p", "2222", "-o", "IdentityFile=key with spaces"}} {
				if e := parse(args); e != nil {
					t.Fatal(args, e)
				}
			}
			for _, args := range [][]string{
				{"work", "-J", "jump"}, {"work", "-p2222"}, {"work", "--"},
				{"work", "--", "-i"}, {"work", "--", "-i", "--"},
				{"work", "--", "--name", "web"}, {"work", "--", "other-host"},
				{"work", "--", "-J", "jump", "extra"}, {"work", "--", "--", "-v"},
				{"work", "--", "-f"}, {"work", "--", "-N"},
			} {
				if e := parse(args); e == nil {
					t.Fatal("accepted", args)
				}
			}
		})
	}
	for _, parse := range []func([]string) error{parsers["mount"], parsers["forward"]} {
		if e := parse([]string{"work", "--", "-D1080"}); e == nil {
			t.Fatal("resource accepted native forwarding")
		}
	}
}

func TestReconnectCommandSSHSeparator(t *testing.T) {
	o := options{host: "work", session: "dev", server: "/opt/sshore", ssh: []string{"-J", "jump"}}
	if got := reconnectCommand(o); got != "sshore attach work dev --server /opt/sshore -- -J jump" {
		t.Fatal(got)
	}
	o.ssh = nil
	if got := reconnectCommand(o); got != "sshore attach work dev --server /opt/sshore" {
		t.Fatal(got)
	}
}

func TestSharedHostValidation(t *testing.T) {
	for _, host := range []string{"host name", "host\tname", "host\x7f", "host\nname"} {
		if _, err := parseOptions([]string{"launch", host}); err == nil {
			t.Fatal("shell accepted", host)
		}
		if _, err := parseFTPOptions([]string{host}); err == nil {
			t.Fatal("mount accepted", host)
		}
		if _, err := parseCopyCommand([]string{"file", host + ":/tmp"}); err == nil {
			t.Fatal("copy accepted", host)
		}
		if _, _, err := parseForwardCommand([]string{"8080", host + ":3000"}); err == nil {
			t.Fatal("forward accepted", host)
		}
	}
}

func TestTargetDispatchRejectsInvalidArgumentsBeforeExecution(t *testing.T) {
	for _, args := range [][]string{
		{"cancel", "mount/demo", "--local"},
		{"cancel", "forward/demo", "--", "-p22"},
		{"cancel", "--all", "--local", "host"},
		{"attach", "host", "name", "extra"},
		{"rename", "host", "old"},
		{"review", "one", "two"},
	} {
		code, err := runTargetCommand(args)
		if code != 2 || err == nil {
			t.Fatalf("%v: code=%d err=%v", args, code, err)
		}
	}
}

func TestCancelAllScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		host     string
		all, bad bool
	}{
		{[]string{"--all"}, "", true, false},
		{[]string{"fedora", "--all"}, "fedora", true, false},
		{[]string{"--all", "fedora", "--", "-p", "2222"}, "fedora", true, false},
		{[]string{"--local", "--all"}, "", true, false},
		{[]string{"fedora", "shell", "--all"}, "", true, true},
		{[]string{"--all", "--all"}, "", true, true},
		{[]string{"--all", "--json"}, "", true, true},
		{[]string{"fedora", "shell", "--", "--all"}, "", false, false},
	} {
		o, all, err := parseCancelAll(tc.args)
		if all != tc.all || (err != nil) != tc.bad || (!tc.bad && all && o.host != tc.host) {
			t.Errorf("%v: host=%q all=%t err=%v", tc.args, o.host, all, err)
		}
	}
}
