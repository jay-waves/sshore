//go:build linux

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/mcp"
)

// bridgeMCP keeps the SSH stream open while each request uses a daemon connection.
func bridgeMCP(c net.Conn, target string) error {
	first := true
	for {
		req, err := mcp.ReadRequest(os.Stdin)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if req.Version != version {
			return fmt.Errorf("release version mismatch")
		}
		if !first {
			c, err = connectRemoteDaemon(target)
			if err != nil {
				return err
			}
		}
		first = false
		err = func() error {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(40 * time.Second))
			if err := writeMessage(c, message{Type: "mcp_request", SoftwareVersion: version, MCPRequest: &req}); err != nil {
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
		}()
		if err != nil {
			return err
		}
	}
}
