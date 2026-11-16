package main

import (
	"github.com/jay-waves/sshore/internal/forward"
	"strings"
	"testing"
)

func TestForwardArguments(t *testing.T) {
	o, err := parseOptions([]string{"launch", "host", "--", "-L", "127.0.0.1:8080:localhost:80"})
	if err != nil || len(o.ssh) != 2 {
		t.Fatal(o, err)
	}
	cmd := sshCommand(o)
	if cmd.Args[7] != "-L" || cmd.Args[8] != o.ssh[1] {
		t.Fatal(cmd.Args)
	}
}

func TestForwardNativeOptions(t *testing.T) {
	o, err := parseOptions([]string{"launch", "host", "--", "-L8080:localhost:80", "-L", "8081:localhost:81"})
	if err != nil || len(o.ssh) != 4 || o.ssh[1] != "8080:localhost:80" || o.ssh[3] != "8081:localhost:81" {
		t.Fatal(o, err)
	}
}

func TestLocalForwardDirections(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		host, flag string
		reverse    bool
	}{
		{[]string{"8080", "work:3000"}, "work", "-L", false},
		{[]string{"work:8080", "3000"}, "work", "-R", true},
		{[]string{"8080", "user@work:3000"}, "user@work", "-L", false},
		{[]string{"[::1]:8080", "3000"}, "::1", "-R", true},
		{[]string{"8080", "user@[::1]:3000"}, "user@::1", "-L", false},
	} {
		f, _, err := parseForwardCommand(tc.args)
		args := forward.SSHArgs(f.transportConfig(), "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), tc.flag+" 127.0.0.1:8080:127.0.0.1:3000") || args[len(args)-1] != tc.host {
			t.Fatal(args)
		}
		f, _, err = parseForwardCommand(tc.args)
		if err != nil || f.Host != tc.host || f.Port != 8080 || f.Target != 3000 || f.Reverse != tc.reverse {
			t.Fatal(f, err)
		}
	}
	for _, args := range [][]string{
		{"work", "3000"}, {"work", "8080", "3000"}, {"3000", "from", "work:8080"},
		{"8080", "3000"}, {"a:8080", "b:3000"},
		{"8080", ":3000"}, {"8080", "-host:3000"},
		{"8080", "work:0"}, {"work:65536", "80"},
		{"8080", "::1:3000"}, {"8080", "[bad]:3000"},
		{"8080", "work:3000", "extra"},
	} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
