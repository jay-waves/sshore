package main

import "testing"

func TestShellVerboseAndSSHBoundary(t *testing.T) {
	o, err := parseOptions([]string{"list", "host", "--verbose", "--", "-v"})
	if err != nil || !o.listDetails || len(o.ssh) != 1 || o.ssh[0] != "-v" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, flag := range []string{"-v", "--details", "--version", "--verbosal"} {
		if _, err := parseOptions([]string{"list", flag}); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestLocalCommandRouting(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		local bool
		name  string
	}{
		{[]string{"shell"}, true, ""},
		{[]string{"shell", "--quiet", "--no-escape"}, true, ""},
		{[]string{"shell", "--name", "work"}, true, "work"},
		{[]string{"shell", "work"}, false, ""},
		{[]string{"shell", "host", "work"}, false, "work"},
		{[]string{"attach", "--local", "work"}, true, "work"},
		{[]string{"attach"}, false, ""},
		{[]string{"list", "--local"}, true, ""},
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
		{"shell", "host", "--name", "work"},
		{"shell", "--name="},
		{"shell", "--local", "--server", "other"},
		{"attach", "--local", "../work"},
		{"cancel", "--local"},
		{"rename", "--local", "work"},
		{"list", "--local", "--json", "--verbose"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
