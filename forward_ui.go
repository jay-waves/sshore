package main

import (
	"fmt"
	"os"
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
		logf(os.Stdout, logInfo, "Remote endpoint: %s:%d. Submitting the local port starts forwarding.\n", cleanText(f.Host), remote)
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
		return fmt.Sprintf("server %s -> local service (choose port in review)", loopback(op.serverPort()))
	}
	return fmt.Sprintf("local listener (choose port in review) -> server %s", loopback(op.serverPort()))
}

// Port always denotes the listener, TargetPort the service, including pending requests.
func (op operation) serverPort() int {
	if op.Reverse {
		return op.Port
	}
	return op.TargetPort
}

func (op operation) localPortPending() bool {
	return op.Kind == "forward" && (op.Port == 0 || op.TargetPort == 0)
}

func resolveLocalPort(op *operation, port int) {
	if op.Reverse {
		op.TargetPort = port
	} else {
		op.Port = port
	}
	op.LocalHint = ""
}

const forwardHelp = `Usage: sshore forward LISTENER SERVICE [options] [-- SSH_OPTIONS]

Connections enter LISTENER and are forwarded to SERVICE.
On your computer, use HOST:PORT for exactly one endpoint and origin:PORT (or a
bare PORT) for the other. HOST is an SSH alias or user@host; bracket IPv6.
Use origin: or : instead of the local endpoint to choose its port interactively.
Use -- -p PORT for the SSH login port; endpoint ports are service/listener ports.

Inside a remote sshore shell, use a bare server port and one origin:[PORT]
(or :) endpoint. This saves a request and detaches. Run sshore review on your
computer to select the local port. origin always means that connection's client,
even when the command runs on the server. A filled local port is an editable review suggestion. A valid submitted port
approves and starts execution;
there is no extra yes prompt. Ctrl-C cancels; Enter accepts a displayed suggestion.
Without a suggestion, empty input retries; invalid ports always retry.

An independent background resource owns each forward. The source shell stays
detached after approval. Manage with sshore list / sshore cancel forward/NAME.
Both endpoints use IPv4 loopback; SSH server policy can override reverse binds.

Local options:
  --name NAME        Resource name (default: host, direction and ports)
  --foreground       Run in this terminal; Ctrl-C stops forwarding
  --retries N        Retry established background connections 0..10 times
                     (default 0; not initial startup or existing connections)
  -h, --help         Show help
Retries cannot ask for credentials or accept new host keys. No reboot restart.
SSH options after --: -p PORT, -i KEY, -J JUMP, -F CONFIG, -l USER, -o OPTION,
-E FILE, -C, -v/-vv/-vvv, -4/-6. Native -L/-R/-D are not accepted here.
Local creation needs a remote Unix shell and cat, not remote sshore installation.

Examples:
  sshore forward origin:8080 work:3000 --name web
  sshore forward work:3000 origin:8080
  sshore forward origin: 3000          Remote request: client listener -> server service
  sshore forward 3000 :               Remote request: server listener -> client service
`
