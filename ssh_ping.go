package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
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

type pingSummary struct {
	connection, minimum, average, maximum time.Duration
	count                                 int
}

func (s pingSummary) render(color bool) string {
	connection := termui.Render(color, termui.Heading, "SSH connection")
	echo := termui.Render(color, termui.Heading, "Echo RTT")
	return fmt.Sprintf("%s: %s\n%s (%d samples): min %s, avg %s, max %s",
		connection, s.connection.Round(time.Microsecond), echo, s.count,
		s.minimum.Round(time.Microsecond), s.average.Round(time.Microsecond), s.maximum.Round(time.Microsecond))
}

func measureSSHPing(ctx context.Context, options sshio.Options, count int, diagnostic io.Writer) (pingSummary, error) {
	if count == 0 {
		count = 10
	}
	start := time.Now()
	client, err := sshio.Dial(ctx, options)
	connectionTime := time.Since(start)
	if err != nil {
		return pingSummary{}, err
	}
	defer client.Close()

	echoContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(echoContext, func() { client.Close() })
	defer stop()

	minimum, average, maximum, err := measureSSHEcho(client, count, diagnostic)
	if err != nil {
		if echoContext.Err() != nil {
			err = echoContext.Err()
		}
		return pingSummary{}, fmt.Errorf("SSH echo: %w", err)
	}
	return pingSummary{connection: connectionTime, minimum: minimum, average: average, maximum: maximum, count: count}, nil
}

func measureSSHEcho(client *sshio.Client, count int, diagnostic io.Writer) (min, avg, max time.Duration, err error) {
	process, err := client.Start("cat", diagnostic)
	if err != nil {
		return 0, 0, 0, err
	}
	var total time.Duration
	var sent, received [1]byte
	for i := range count {
		sent[0] = byte(i)
		start := time.Now()
		if _, err := process.Write(sent[:]); err != nil {
			return 0, 0, 0, err
		}
		if _, err := io.ReadFull(process, received[:]); err != nil {
			return 0, 0, 0, err
		}
		elapsed := time.Since(start)
		if received != sent {
			return 0, 0, 0, fmt.Errorf("remote command returned an unexpected byte")
		}
		if i == 0 || elapsed < min {
			min = elapsed
		}
		if elapsed > max {
			max = elapsed
		}
		total += elapsed
	}
	if err := process.CloseWrite(); err != nil {
		return 0, 0, 0, err
	}
	if err := process.Wait(); err != nil {
		return 0, 0, 0, err
	}
	return min, total / time.Duration(count), max, nil
}
