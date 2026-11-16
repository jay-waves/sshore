package main

import (
	"fmt"
	"os"

	"github.com/jay-waves/sshore/internal/termui"
)

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
