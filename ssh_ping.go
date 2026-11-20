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
	_, err = fmt.Fprintln(os.Stdout, summary)
	return 0, err
}

func measureSSHPing(ctx context.Context, options sshio.Options, count int, diagnostic io.Writer) (string, error) {
	if count == 0 {
		count = 10
	}
	start := time.Now()
	client, err := sshio.Dial(ctx, options)
	connectionTime := time.Since(start)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("SSH echo: %w", err)
	}
	return fmt.Sprintf("SSH connection: %s\nEcho RTT (%d samples): min %s, avg %s, max %s",
		connectionTime.Round(time.Microsecond), count,
		minimum.Round(time.Microsecond), average.Round(time.Microsecond), maximum.Round(time.Microsecond)), nil
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
