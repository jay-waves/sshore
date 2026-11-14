package main

import (
	"strings"
	"testing"
)

func TestForwardArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"3000"}, {"7777:6666"}, {"7777:host:6666"}, {"to"}, {"from"}, {"to", "from"}, {"0", "to"}, {"from", "65536"}, {"80", "from", "0"}, {"65536", "to", "80"}, {"80", "on", "90"}, {"--stop", "80"}, {"--list", "80"}} {
		if _, e := parseForward(args); e == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, tc := range []struct {
		args         []string
		port, target int
		reverse      bool
	}{
		{[]string{"3000", "from", "8080"}, 8080, 3000, false},
		{[]string{"8080", "to", "3000"}, 8080, 3000, true},
		{[]string{"--stop", "8080", "to", "3000"}, 8080, 3000, true},
	} {
		m, e := parseForward(tc.args)
		if e != nil || m.Port != tc.port || m.RemotePort != tc.target || m.Reverse != tc.reverse {
			t.Fatalf("%v: %+v %v", tc.args, m, e)
		}
	}
	if _, e := parseForward([]string{"--list"}); e != nil {
		t.Fatal(e)
	}

	o, err := parseOptions([]string{"new", "host", "-L", "127.0.0.1:8080:localhost:80"})
	if err != nil || len(o.ssh) != 2 {
		t.Fatal(o, err)
	}
	cmd := sshCommand(o)
	if cmd.Args[7] != "-L" || cmd.Args[8] != o.ssh[1] {
		t.Fatal(cmd.Args)
	}
}

func TestForwardNativeOptions(t *testing.T) {
	o, err := parseOptions([]string{"new", "host", "-L8080:localhost:80", "-L", "8081:localhost:81"})
	if err != nil || len(o.ssh) != 4 || o.ssh[1] != "8080:localhost:80" || o.ssh[3] != "8081:localhost:81" {
		t.Fatal(o, err)
	}
}

func TestForwardOmittedPorts(t *testing.T) {
	for _, direction := range []string{"from", "to"} {
		for _, operands := range [][]string{{direction, "3000"}, {"3000", direction}, {"3000", direction, "3000"}} {
			for _, stop := range []bool{false, true} {
				args := append([]string(nil), operands...)
				action := "add"
				if stop {
					args = append([]string{"--stop"}, args...)
					action = "stop"
				}
				m, e := parseForward(args)
				if e != nil || m.Port != 3000 || m.RemotePort != 3000 || m.Reverse != (direction == "to") || m.Action != action {
					t.Fatalf("%v: %+v %v", args, m, e)
				}
				if !stop {
					ssh, e := localForwardArgs(append([]string{"work"}, operands...))
					if e != nil {
						t.Fatal(e)
					}
					flag := "-R"
					if direction == "to" {
						flag = "-L"
					}
					if !strings.Contains(strings.Join(ssh, " "), flag+" 127.0.0.1:3000:127.0.0.1:3000") {
						t.Fatal(ssh)
					}
				}
			}
		}
	}
}
