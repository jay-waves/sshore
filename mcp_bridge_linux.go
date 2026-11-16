//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/mcp"
)

// bridgeMCP proxies one framed request over the account's daemon socket.
func bridgeMCP(c net.Conn) error {
	req, err := mcp.ReadRequest(os.Stdin)
	if err != nil {
		return err
	}
	if req.Version != version {
		return fmt.Errorf("release version mismatch")
	}
	_ = c.SetDeadline(time.Now().Add(40 * time.Second))
	if err = writeMessage(c, message{Type: "mcp_request", SoftwareVersion: version, MCPRequest: &req}); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "mcp_response" || reply.SoftwareVersion != version || reply.MCPResponse == nil {
		return fmt.Errorf("invalid daemon response")
	}
	return mcp.WriteResponse(os.Stdout, *reply.MCPResponse)
}
