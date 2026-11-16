package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"golang.org/x/term"
)

func sshConnectionOptions(o options) sshio.Options {
	return sshio.Options{Host: o.host, Command: "export SSHORE_DIRECT_TRANSPORT=1; " + remoteCommand(o), Args: o.ssh, Environment: o.environment, Directory: o.sshDirectory, Confirm: confirmSSHHost}
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
	return connection
}
