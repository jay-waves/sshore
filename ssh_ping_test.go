package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"
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

func TestPingSampleGroups(t *testing.T) {
	var stream bytes.Buffer
	pauses := 0
	batches, err := measurePingSamples(context.Background(), &stream, 21, func(context.Context) error { pauses++; return nil })
	if err != nil || len(batches) != 3 || pauses != 2 {
		t.Fatalf("groups=%+v pauses=%d err=%v", batches, pauses, err)
	}
	for i, count := range []int{10, 10, 1} {
		if batches[i].count != count || batches[i].minimum > batches[i].maximum {
			t.Fatalf("group %d: %+v", i, batches[i])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitPingInterval(ctx); !errors.Is(err, context.Canceled) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancelled wait: %v", err)
	}
}
