package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/tmux"
	"golang.org/x/term"
)

func sshConnectionOptions(o options) sshio.Options {
	return sshio.Options{Host: o.host, Command: "export SSHORE_DIRECT_TRANSPORT=1; " + remoteCommand(o), Args: o.ssh, Environment: o.environment, Directory: o.sshDirectory, Confirm: confirmSSHHost, Passphrase: unlockSSHKey}
}
func confirmSSHHost(ctx context.Context, prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("SSH host trust requires an interactive terminal")
	}
	input, err := newLocalInput()
	if err != nil {
		return false, err
	}
	defer input.Close()
	stop := context.AfterFunc(ctx, func() { input.Close() })
	defer stop()
	answer, err := input.Read(cleanText(prompt)+" Type yes: ", false)
	return strings.TrimSpace(answer) == "yes", err
}
func unlockSSHKey(ctx context.Context, identity string) ([]byte, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, fmt.Errorf("SSH key unlock requires an interactive terminal")
	}
	secret, err := readSSHSecret(ctx, "Passphrase for "+cleanText(identity)+": ")
	if err != nil {
		return nil, err
	}
	if len(secret) > 4096 {
		clear(secret)
		return nil, fmt.Errorf("SSH passphrase is too long")
	}
	return secret, nil
}

// Reuse the joined console reader so cancellation cannot leave stdin owned by
// an abandoned password prompt. Secret bytes never enter readline or history.
func readSSHSecret(parent context.Context, prompt string) ([]byte, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	restore, err := tmux.RawConsole()
	if err != nil {
		return nil, err
	}
	defer restore()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type input struct {
		data []byte
		err  error
	}
	incoming := make(chan input)
	stop, err := tmux.StartKeyboard(ctx, func(data []byte, err error) {
		select {
		case incoming <- input{data, err}:
		case <-ctx.Done():
			clear(data)
		}
	})
	if err != nil {
		return nil, err
	}
	defer func() { cancel(); stop() }()
	if _, err := fmt.Fprint(os.Stderr, cleanText(prompt)); err != nil {
		return nil, err
	}
	defer fmt.Fprint(os.Stderr, "\r\n")
	secret := make([]byte, 0, 4096)
	accepted := false
	defer func() {
		if !accepted {
			clear(secret)
		}
	}()
	escape := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event := <-incoming:
			for _, key := range event.data {
				if escape > 0 {
					if escape == 1 && (key == '[' || key == 'O') {
						escape = 2
					} else if escape == 1 || key >= '@' && key <= '~' {
						escape = 0
					}
					continue
				}
				switch key {
				case '\r', '\n':
					clear(event.data)
					accepted = true
					return secret, nil
				case 3:
					clear(event.data)
					return nil, context.Canceled
				case 4:
					clear(event.data)
					return nil, io.EOF
				case 8, 127:
					_, size := utf8.DecodeLastRune(secret)
					clear(secret[len(secret)-size:])
					secret = secret[:len(secret)-size]
				case 21:
					clear(secret)
					secret = secret[:0]
				case 27:
					escape = 1
				default:
					if len(secret) >= 4096 {
						clear(event.data)
						return nil, fmt.Errorf("SSH passphrase is too long")
					}
					secret = append(secret, key)
				}
			}
			clear(event.data)
			if event.err != nil {
				return nil, event.err
			}
		}
	}
}

func brokerSSHPassphrase(env []string) func(context.Context, string) ([]byte, error) {
	address, token := localEnvironmentValue(env, "SSHORE_CONFIRM_ADDR"), localEnvironmentValue(env, "SSHORE_CONFIRM_TOKEN")
	if address == "" {
		return nil
	}
	return func(ctx context.Context, identity string) ([]byte, error) {
		response, err := resourceHTTPContext(ctx, address, token, "POST", "/unlock", strings.NewReader(identity))
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, fmt.Errorf("SSH key unlock unavailable")
		}
		secret, err := io.ReadAll(io.LimitReader(response.Body, 4097))
		if err != nil || len(secret) > 4096 {
			clear(secret)
			return nil, fmt.Errorf("invalid SSH unlock response")
		}
		return secret, nil
	}
}

func brokerSSHConfirmation(env []string) func(context.Context, string) (bool, error) {
	address, token := localEnvironmentValue(env, "SSHORE_CONFIRM_ADDR"), localEnvironmentValue(env, "SSHORE_CONFIRM_TOKEN")
	if address == "" {
		return nil
	}
	return func(ctx context.Context, prompt string) (bool, error) {
		response, err := resourceHTTPContext(ctx, address, token, "POST", "/ask", strings.NewReader(prompt))
		if err != nil {
			return false, err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return false, fmt.Errorf("SSH host confirmation unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 32))
		return string(data) == "yes", err
	}
}

func sshioOptionsForSpawn(o options, req localDaemonRequest) sshio.Options {
	connection := sshConnectionOptions(o)
	connection.Environment = req.Env
	connection.Confirm = brokerSSHConfirmation(req.Env)
	connection.Passphrase = brokerSSHPassphrase(req.Env)
	connection.IdentityCache = req.identityCache
	return connection
}
