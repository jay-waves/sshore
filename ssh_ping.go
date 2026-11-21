package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func runPing(args []string) (int, error) {
	parsed, err := parseCLI(args, "ping")
	if err != nil {
		return 2, err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{
		Host: parsed.operand(0), Args: parsed.ssh, Diagnostic: os.Stderr,
		Confirm: confirmSSHHost, Secret: promptSSHSecret,
	}
	summary, err := measureSSHPing(ctx, connection, parsed.integer("--count"), os.Stderr)
	if err != nil {
		return 1, err
	}
	_, err = fmt.Fprintln(os.Stdout, summary.render(termui.ColorEnabled(os.Stdout)))
	return 0, err
}

const pingBatchSize = 10

type pingBatch struct {
	minimum, total, maximum time.Duration
	count                   int
}

func (b *pingBatch) add(elapsed time.Duration) {
	if b.count == 0 || elapsed < b.minimum {
		b.minimum = elapsed
	}
	if elapsed > b.maximum {
		b.maximum = elapsed
	}
	b.total += elapsed
	b.count++
}

type pingSummary struct {
	connection time.Duration
	batches    []pingBatch
}

func (s pingSummary) render(color bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s: %s", termui.Render(color, termui.Heading, "SSH connection"), s.connection.Round(time.Microsecond))
	for i, batch := range s.batches {
		fmt.Fprintf(&out, "\n%s %d (%d samples): min %s, avg %s, max %s", termui.Render(color, termui.Heading, "Echo RTT"), i+1, batch.count, batch.minimum.Round(time.Microsecond), (batch.total / time.Duration(batch.count)).Round(time.Microsecond), batch.maximum.Round(time.Microsecond))
	}
	return out.String()
}

func measureSSHPing(ctx context.Context, options sshio.Options, count int, diagnostic io.Writer) (pingSummary, error) {
	if count == 0 {
		count = 30
	}
	start := time.Now()
	client, err := sshio.Dial(ctx, options)
	connectionTime := time.Since(start)
	if err != nil {
		return pingSummary{}, err
	}
	defer client.Close()

	echoContext, cancel := context.WithTimeout(ctx, 30*time.Second+time.Duration((count-1)/pingBatchSize)*time.Second)
	defer cancel()
	stop := context.AfterFunc(echoContext, func() { client.Close() })
	defer stop()

	batches, err := measureSSHEcho(echoContext, client, count, diagnostic)
	if err != nil {
		if echoContext.Err() != nil {
			err = echoContext.Err()
		}
		return pingSummary{}, fmt.Errorf("SSH echo: %w", err)
	}
	return pingSummary{connection: connectionTime, batches: batches}, nil
}

func measureSSHEcho(ctx context.Context, client *sshio.Client, count int, diagnostic io.Writer) ([]pingBatch, error) {
	process, err := client.Start("cat", diagnostic)
	if err != nil {
		return nil, err
	}
	batches, err := measurePingSamples(ctx, process, count, waitPingInterval)
	if err != nil {
		return nil, err
	}
	if err := process.CloseWrite(); err != nil {
		return nil, err
	}
	if err := process.Wait(); err != nil {
		return nil, err
	}
	return batches, nil
}

func waitPingInterval(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func measurePingSamples(ctx context.Context, stream io.ReadWriter, count int, pause func(context.Context) error) ([]pingBatch, error) {
	batches := make([]pingBatch, 0, (count+pingBatchSize-1)/pingBatchSize)
	var batch pingBatch
	var sent, received [1]byte
	for i := range count {
		sent[0] = byte(i)
		start := time.Now()
		if _, err := stream.Write(sent[:]); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(stream, received[:]); err != nil {
			return nil, err
		}
		elapsed := time.Since(start)
		if received != sent {
			return nil, fmt.Errorf("remote command returned an unexpected byte")
		}
		batch.add(elapsed)
		if batch.count == pingBatchSize || i == count-1 {
			batches = append(batches, batch)
			batch = pingBatch{}
			if i+1 < count {
				if err := pause(ctx); err != nil {
					return nil, err
				}
			}
		}
	}
	return batches, nil
}
