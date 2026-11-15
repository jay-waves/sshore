package main

import (
	"bytes"
	"strings"
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
		{[]string{"launch", "--name", "work"}, true, "work"},
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

func TestHiddenShellAliasUsesSpawn(t *testing.T) {
	for _, args := range [][]string{{"shell"}, {"shell", "--name", "dev", "--detach"}, {"shell", "host", "dev", "--", "-p2222"}} {
		got, err := parseOptions(args)
		if err != nil || got.command != "spawn" {
			t.Fatalf("%v: %+v %v", args, got, err)
		}
		if !got.local && !strings.Contains(remoteCommand(got), shellQuote("spawn")) {
			t.Fatal("alias did not select remote spawn")
		}
	}
	if _, ok := commandHelp["shell"]; ok {
		t.Fatal("alias is advertised as a command")
	}
	var alias, canonical bytes.Buffer
	if err := showHelp([]string{"shell"}, &alias); err != nil {
		t.Fatal(err)
	}
	if err := showHelp([]string{"spawn"}, &canonical); err != nil {
		t.Fatal(err)
	}
	if alias.String() != canonical.String() {
		t.Fatal("alias help differs from spawn")
	}
	for _, manual := range append([]string{help}, mapHelpValues()...) {
		if strings.Contains(manual, "sshore shell [") || strings.Contains(manual, "shell/NAME") {
			t.Fatal("manual advertises hidden alias")
		}
	}
}

func mapHelpValues() []string {
	var values []string
	for _, value := range commandHelp {
		values = append(values, value)
	}
	return values
}
