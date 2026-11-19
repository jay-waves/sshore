package main

import (
	"context"
	"io"
	"os"

	sshoremcp "github.com/jay-waves/sshore/internal/mcp"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func dialMCP(ctx context.Context, host string) (io.ReadWriteCloser, error) {
	if err := validSSHDestination(host); err != nil {
		return nil, err
	}
	o := options{command: "mcp-agent", host: host, server: "sshore"}
	connection := sshConnectionOptions(o)
	connection.Confirm = nil
	connection.Secret = nil
	pipes, err := transportio.Start(ctx, connection, os.Stderr)
	if err != nil {
		return nil, err
	}
	if _, err = negotiateSetup(pipes, pipes, host, false); err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = pipes.CloseAndWait()
		return nil, err
	}
	// The SSH exec stream already follows ctx. The pool owns cancellation
	// for published transports and closes this wrapper to join exec.
	return wire.OwnTransport(context.Background(), pipes, func() { _ = pipes.Wait() }), nil
}
func runMCP(args []string) (int, error) {
	if _, err := parseCLI(args, "mcp"); err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	client := &sshoremcp.Client{Version: version, Dial: dialMCP}
	defer client.Shutdown()
	server := sshoremcp.New(version, client)
	return 0, server.Run(context.Background(), &sdk.StdioTransport{})
}
