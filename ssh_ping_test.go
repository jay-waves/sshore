package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/tests/testutil/sshtest"
	cryptossh "golang.org/x/crypto/ssh"
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

func TestPingResolvesMatchCommand(t *testing.T) {
	path := sshtest.Start(t, sshtest.Options{Exec: func(_ context.Context, command string, stream cryptossh.Channel) {
		if command == "cat" {
			_, _ = io.Copy(stream, stream)
			sshtest.Exit(stream, 0)
		}
	}})
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config = append(config, []byte("Match command cat\n ProxyCommand none\nMatch all\n ProxyCommand unused\n")...)
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	summary, err := measureSSHPing(ctx, sshio.Options{Host: "fixture", Args: []string{"-F", path}}, 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.batches) != 1 || summary.batches[0].count != 1 {
		t.Fatalf("ping reported %+v", summary)
	}
}

func TestPingSummaryStylesPreserveMeasurements(t *testing.T) {
	summary := pingSummary{connection: 1234 * time.Microsecond, batches: []pingBatch{{minimum: 7 * time.Microsecond, total: 24 * time.Microsecond, maximum: 9 * time.Microsecond, count: 3}}}
	want := "SSH connection: 1.234ms\nEcho RTT 1 (3 samples): min 7µs, avg 8µs, max 9µs"
	plain, styled := summary.render(false), summary.render(true)
	if plain != want {
		t.Fatalf("plain summary: %q", plain)
	}
	if !strings.Contains(styled, "\x1b[") || ansi.Strip(styled) != want {
		t.Fatalf("styled summary: %q", styled)
	}
}

func TestPingSummaryShowsEachGroup(t *testing.T) {
	group := pingBatch{minimum: time.Millisecond, total: 20 * time.Millisecond, maximum: 3 * time.Millisecond, count: 10}
	summary := pingSummary{connection: time.Millisecond, batches: []pingBatch{group, group, group}}
	text := summary.render(false)
	for _, want := range []string{"SSH connection: 1ms", "Echo RTT 1 (10 samples)", "Echo RTT 2 (10 samples)", "Echo RTT 3 (10 samples)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Count(text, "\n") != 3 {
		t.Fatalf("wrong row count: %q", text)
	}
}
