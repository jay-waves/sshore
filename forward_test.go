package main

import (
	"testing"
)

func TestForwardArguments(t *testing.T) {
	o, err := parseOptions([]string{"new", "host", "--", "-L", "127.0.0.1:8080:localhost:80"})
	if err != nil || len(o.ssh) != 2 {
		t.Fatal(o, err)
	}
	cmd := sshCommand(o)
	if cmd.Args[7] != "-L" || cmd.Args[8] != o.ssh[1] {
		t.Fatal(cmd.Args)
	}
}

func TestForwardNativeOptions(t *testing.T) {
	o, err := parseOptions([]string{"new", "host", "--", "-L8080:localhost:80", "-L", "8081:localhost:81"})
	if err != nil || len(o.ssh) != 4 || o.ssh[1] != "8080:localhost:80" || o.ssh[3] != "8081:localhost:81" {
		t.Fatal(o, err)
	}
}
