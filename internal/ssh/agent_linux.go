package ssh

import (
	"context"
	"net"
	"os/exec"
)

func connectAgent(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
func defaultAgent() string { return "" }

func hideConfigCommand(cmd *exec.Cmd) {}

func (c *connectionConfig) expandSystemPath(path string) (string, error) { return path, nil }
