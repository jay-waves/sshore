package main

import (
	"bytes"
	"testing"
)

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
		{[]string{"launch"}, true, ""},
		{[]string{"launch", "--quiet", "--no-escape"}, true, ""},
		{[]string{"launch", "--local", "work"}, true, "work"},
		{[]string{"launch", "work"}, false, ""},
		{[]string{"launch", "host", "work"}, false, "work"},
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
		{"launch", "host", "--name", "work"},
		{"launch", "--name="},
		{"launch", "--local", "--server", "other"},
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

func TestRemovedShellForms(t *testing.T) {
	for _, args := range [][]string{{"shell"}, {"launch", "--name", "dev"}, {"spawn", "--name=dev"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"shell"}, &out); err == nil {
		t.Fatal("accepted removed alias help")
	}
}

func mapHelpValues() []string {
	var values []string
	for _, value := range commandHelp {
		values = append(values, value)
	}
	return values
}

func TestRenameOptions(t *testing.T) {
	for _, args := range [][]string{{"rename", "host", "old", "new"}, {"rename", "host", "old", "new", "--", "-p22"}} {
		o, err := parseOptions(args)
		if err != nil || o.session != "old" || o.newName != "new" {
			t.Fatal(o, err)
		}
	}
	for _, args := range [][]string{{"rename", "host", "old"}, {"rename", "host", "old", "new", "extra"}, {"rename", "host", "old", "bad/name"}, {"rename", "host", "old", "new", "--quiet"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
