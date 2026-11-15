package main

import "testing"

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
