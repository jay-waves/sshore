package main

import "testing"

func TestExecArguments(t *testing.T) {
	for _, command := range []string{"uname -a", "printf '%s' --help", "--help", "echo x -- -p 22"} {
		parsed, err := parseCLI([]string{"work", command}, "exec")
		if err != nil || len(parsed.operands) != 2 || parsed.operands[1] != command {
			t.Fatalf("command %q: %+v, %v", command, parsed, err)
		}
	}
	for _, args := range [][]string{nil, {"work"}, {"work", "echo", "extra"}, {"-host", "echo"}} {
		if _, err := parseCLI(args, "exec"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
