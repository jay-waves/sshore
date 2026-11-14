package main

// Independent SSH workers own forwarding; no listener belongs to a shell attachment.
import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

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

// A qualified endpoint identifies the peer; the first endpoint always listens.
func parseForwardEndpoints(args []string) (string, int, int, bool, error) {
	fail := func() (string, int, int, bool, error) {
		return "", 0, 0, false, usage("invalid port mapping; use forward origin:PORT HOST:PORT or forward HOST:PORT origin:PORT")
	}
	if len(args) != 2 {
		return fail()
	}
	endpoint := func(s string) (string, int, error) {
		if originPlaceholder(s) {
			return "", 0, nil
		}
		if strings.HasPrefix(s, "origin:") {
			n, e := parsePort(originHint(s))
			return "", n, e
		}
		if !strings.Contains(s, ":") {
			n, e := parsePort(s)
			return "", n, e
		}
		i := strings.LastIndex(s, ":")
		host, err := endpointHost(s[:i])
		if err != nil {
			return "", 0, err
		}
		n, e := parsePort(s[i+1:])
		return host, n, e
	}
	left, port, err := endpoint(args[0])
	if err != nil {
		return "", 0, 0, false, err
	}
	right, target, err := endpoint(args[1])
	if err != nil {
		return "", 0, 0, false, err
	}
	if (left == "") == (right == "") {
		return fail()
	}
	host := right
	if left != "" {
		host = left
	}
	return host, port, target, left != "", nil
}

func describeForward(port, target int, reverse bool) string {
	listener, service := "your computer", "server"
	if reverse {
		listener, service = service, listener
	}
	return fmt.Sprintf("%s %s -> %s %s", listener, loopback(port), service, loopback(target))
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
