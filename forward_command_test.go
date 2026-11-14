package main

import (
	"strings"
	"testing"
)

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
		args := forwardSSHArgs(f, false)
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
