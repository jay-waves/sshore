package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/jay-waves/sshore/internal/forward"
	"github.com/jay-waves/sshore/internal/termui"
)

// Independent SSH workers own forwarding; no listener belongs to a shell attachment.

func describeForward(port, target int, reverse bool) string {
	listener, service := "your computer", "server"
	if reverse {
		listener, service = service, listener
	}
	return fmt.Sprintf("%s %s -> %s %s", listener, loopback(port), service, loopback(target))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Local requests default to independent background workers; --foreground is
// available for diagnostics. Remote shell
// requests inherit the session and require local approval. Port commands resolve
// server/local ports into fixed listener and target identities. Native SSH
// configuration/authentication remains effective.

// Remote helper selection is application policy; the engine receives opaque command text.
func forwardRemoteCommand(f forwardConfig) string {
	if f.ApprovalID != "" {
		return approvedForwardCommand(f)
	}
	return forwardWatchCommand(f.Server)
}

func runForwardForeground(f forwardConfig) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprintf(os.Stderr, "Starting %s (%s); Ctrl-C stops it.\n", describeForward(f.Port, f.Target, f.Reverse), cleanText(f.Host))
	return forward.RunForeground(ctx, sshExecutable(), f.transportConfig(), os.Stdin, os.Stdout, os.Stderr)
}

func runEndpointForward(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if _, err := remoteForwardRequest(args); err != nil {
			return 2, err
		}
		return 0, serverCommand(append([]string{"forward"}, args...))
	}
	f, foreground, err := parseForwardCommand(args)
	if err != nil {
		return 2, err
	}
	if f.Port == 0 || f.Target == 0 {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		defer input.Close()
		label := "Local listening port"
		remote := f.Target
		if f.Reverse {
			label = "Local service port"
			remote = f.Port
		}
		termui.Log(os.Stdout, termui.Info, "Remote %s:%d. Submit local port to forward.\n", cleanText(f.Host), remote)
		port, err := readSelectedPort(input, label, "", "forward cancelled")
		if err != nil {
			return 1, err
		}
		if f.Reverse {
			f.Target = port
		} else {
			f.Port = port
		}
		if err = input.Close(); err != nil {
			return 1, err
		}
	}
	if foreground {
		return runForwardForeground(f)
	}
	return startForwardSession(f)
}

func pendingPortDescription(op operation) string {
	if op.Reverse {
		return fmt.Sprintf("server %s -> local service (choose port in review)", loopback(op.ServerPort()))
	}
	return fmt.Sprintf("local listener (choose port in review) -> server %s", loopback(op.ServerPort()))
}

func resolveLocalPort(op *operation, port int) {
	if op.Reverse {
		op.TargetPort = port
	} else {
		op.Port = port
	}
	op.LocalHint = ""
}
