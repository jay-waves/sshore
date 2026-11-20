package main

import (
	"slices"
	"testing"
)

func TestPingArguments(t *testing.T) {
	args, err := parseCLI([]string{"user@work:", "--count", "5", "--port", "2222", "--", "-J", "jump"}, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if args.operand(0) != "user@work" || args.integer("--count") != 5 || !slices.Equal(args.ssh, []string{"-p", "2222", "-J", "jump"}) {
		t.Fatalf("unexpected parsed arguments: %+v", args)
	}
	for _, input := range [][]string{
		nil, {"here:"}, {"work:session"}, {"work:", "--count", "0"}, {"work:", "--count", "1001"},
		{"work:", "--", "-L", "1234:localhost:1234"},
		{"work:", "-c", "5"},
	} {
		if _, err := parseCLI(input, "ping"); err == nil {
			t.Errorf("accepted invalid arguments %q", input)
		}
	}
}
