package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	sshoremcp "github.com/jay-waves/sshore/internal/mcp"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpConnection struct {
	pipes *transportio.Process
	stop  func()
	once  sync.Once
}

func (c *mcpConnection) Read(b []byte) (int, error)  { return c.pipes.Read(b) }
func (c *mcpConnection) Write(b []byte) (int, error) { return c.pipes.Write(b) }
func (c *mcpConnection) Close() error                { c.once.Do(c.stop); return nil }

func dialMCP(ctx context.Context, host string) (io.ReadWriteCloser, error) {
	if err := validSSHDestination(host); err != nil {
		return nil, err
	}
	o := options{command: "mcp-agent", host: host, server: "sshore"}
	connection := sshConnectionOptions(o)
	connection.Confirm = nil
	connection.Passphrase = nil
	pipes, err := transportio.Start(ctx, connection, os.Stderr)
	if err != nil {
		return nil, err
	}
	c := &mcpConnection{pipes: pipes}
	c.stop = func() { pipes.Close(); pipes.Wait() }
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	err = negotiateSetupUsing(ctx, pipes.Output, pipes.Input, &o, setupClientUI{
		Log: os.Stderr,
		Choose: func(message) (string, error) {
			return "", fmt.Errorf("remote setup needs user review; run sshore launch %s and reconnect", host)
		},
	})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if o.plain {
		_ = c.Close()
		return nil, fmt.Errorf("remote is in recovery mode")
	}
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
func runMCP(args []string) (int, error) {
	if _, err := parseCLI(args, "mcp"); err != nil {
		return 2, err
	}
	server := sshoremcp.New(version, &sshoremcp.Client{Version: version, Dial: dialMCP})
	return 0, server.Run(context.Background(), &sdk.StdioTransport{})
}
