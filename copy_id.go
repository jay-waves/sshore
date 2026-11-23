package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

func runCopyID(args []string) (int, error) {
	parsed, err := parseCLI(args, "copy-id")
	if err != nil {
		return 2, err
	}
	host := parsed.operands[0]
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{Host: host, Args: parsed.ssh, Confirm: confirmSSHHost, Secret: promptSSHSecret}
	selectKey := func(paths []string) (int, error) {
		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
			return -1, fmt.Errorf("multiple IdentityFile keys require an interactive terminal")
		}
		choices := make([]string, len(paths))
		for i, path := range paths {
			choices[i] = cleanText(path)
			fmt.Fprintf(os.Stderr, "%d  %s\n", i+1, choices[i])
		}
		for {
			answer, err := readSSHInput(ctx, func(input *termui.Input) (string, error) {
				return input.ReadChoice("Public key: ", choices)
			})
			if err != nil {
				return -1, err
			}
			if index := slices.Index(choices, answer); index >= 0 {
				return index, nil
			}
			if number, err := strconv.Atoi(strings.TrimSpace(answer)); err == nil && number >= 1 && number <= len(paths) {
				return number - 1, nil
			}
			fmt.Fprintln(os.Stderr, "Choose a listed public key by number or press Tab to complete.")
		}
	}
	if err := sshio.CopyID(ctx, connection, selectKey, os.Stderr); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stdout, "Public key is present in ~/.ssh/authorized_keys on %s.\n", cleanText(host))
	return 0, nil
}
