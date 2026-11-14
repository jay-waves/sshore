package main

// Forward operands are resolved relative to the requesting shell, then stored
// as fixed listener/destination identities for local approval. Independent SSH
// workers own forwarding; no listener belongs to a shell attachment.
import (
	"fmt"
	"net"
	"strconv"
)

type forwardSpec struct {
	Reverse    bool   `json:"reverse,omitempty"`
	Port       int    `json:"port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

func normalizeForward(port int, host string, remotePort int) forwardSpec {
	if host == "" {
		host = "127.0.0.1"
	}
	if remotePort == 0 {
		remotePort = port
	}
	return forwardSpec{Port: port, RemoteHost: host, RemotePort: remotePort}
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, usage("port must be an integer from 1 to 65535")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, usage("port must be an integer from 1 to 65535")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, usage("port must be an integer from 1 to 65535")
	}
	return n, nil
}

// parseForward interprets operands in the requesting remote shell's frame.
// Wire fields are then fixed: Reverse means server listener -> local client.
// Port is always the listener; RemotePort is always the approved destination.
func parseForward(args []string) (message, error) {
	m := message{Type: "forward", Action: "add", RemoteHost: "127.0.0.1"}
	if len(args) == 1 && args[0] == "--list" {
		m.Action = "list"
		return m, nil
	}
	if len(args) > 0 && args[0] == "--stop" {
		m.Action = "stop"
		args = args[1:]
	}
	// A missing endpoint copies the explicit port; direction never defaults.
	if len(args) == 2 {
		if args[0] == "from" || args[0] == "to" {
			args = []string{args[1], args[0], args[1]}
		} else if args[1] == "from" || args[1] == "to" {
			args = []string{args[0], args[1], args[0]}
		}
	}
	if len(args) != 3 || (args[1] != "from" && args[1] != "to") {
		return m, usage("use forward CURRENT_PORT from|to PEER_PORT (at least one port required); --stop uses the same operands")
	}
	current, e := parsePort(args[0])
	if e != nil {
		return m, e
	}
	peer, e := parsePort(args[2])
	if e != nil {
		return m, e
	}
	if args[1] == "to" {
		m.Port = current
		m.RemotePort = peer
		m.Reverse = true
	} else {
		m.Port = peer
		m.RemotePort = current
	}
	return m, nil
}

func describeForward(port int, host string, target int, reverse bool) string {
	spec := normalizeForward(port, host, target)
	if reverse {
		return fmt.Sprintf("server %s -> your computer %s", loopback(spec.Port), net.JoinHostPort(spec.RemoteHost, strconv.Itoa(spec.RemotePort)))
	}
	return fmt.Sprintf("your computer %s -> server %s", loopback(spec.Port), net.JoinHostPort(spec.RemoteHost, strconv.Itoa(spec.RemotePort)))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
