package main

import (
	"github.com/jay-waves/sshore/internal/forward"
	"strconv"
	"strings"
)

func parsePort(s string) (int, error) {
	n, err := forward.ParsePort(s)
	if err != nil {
		return 0, usage("%s", err)
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

func parseForwardCommand(args []string) (forwardConfig, bool, error) {
	var f forwardConfig
	parsed, err := parseCLI(args, []cliFlag{{"--name", true}, {"--retries", true}, {"--foreground", false}}, true, false)
	if err != nil {
		return f, false, err
	}
	f.SSH, f.Name = parsed.ssh, parsed.values["--name"]
	if parsed.has("--name") {
		if err := validSession(f.Name); err != nil {
			return f, false, err
		}
	}
	if parsed.has("--retries") {
		f.Retries, err = strconv.Atoi(parsed.values["--retries"])
		if err != nil || f.Retries < 0 || f.Retries > 10 {
			return f, false, usage("--retries must be between 0 and 10")
		}
	}
	foreground := parsed.has("--foreground")
	operands := parsed.operands
	if foreground && f.Retries != 0 {
		return f, false, usage("--retries applies only to background forwards")
	}
	f.Host, f.Port, f.Target, f.Reverse, err = parseForwardEndpoints(operands)
	return f, foreground, err
}
