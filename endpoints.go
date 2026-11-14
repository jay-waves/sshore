package main

import (
	"net"
	"strings"
)

// A placeholder always names the attached client, never the current shell.
func originPlaceholder(s string) bool { return s == ":" || s == "origin:" }

func originEndpoint(s string) bool { return s == ":" || strings.HasPrefix(s, "origin:") }
func originHint(s string) string {
	if s == ":" {
		return ""
	}
	return strings.TrimPrefix(s, "origin:")
}

func remoteCopyRequest(args []string) (string, []string, string, error) {
	fail := func() (string, []string, string, error) {
		return "", nil, "", usage("use cp SOURCE... origin:[PATH] or cp origin:[PATH] DIRECTORY")
	}
	if len(args) < 2 {
		return fail()
	}
	kind, paths, hint := "download", args[:len(args)-1], originHint(args[len(args)-1])
	if originEndpoint(args[0]) {
		if len(args) != 2 || originEndpoint(args[1]) {
			return fail()
		}
		kind, paths, hint = "upload", args[1:], originHint(args[0])
	} else if !originEndpoint(args[len(args)-1]) {
		return fail()
	}
	if len(hint) > 4096 || strings.ContainsAny(hint, "\x00\r\n") {
		return fail()
	}
	for _, value := range paths {
		endpoint, err := parseCopyEndpoint(value)
		if err != nil || endpoint.stream || originEndpoint(value) || endpoint.host != "" {
			return fail()
		}
	}
	return kind, paths, hint, nil
}

func remoteForwardRequest(args []string) (operation, error) {
	op := operation{Kind: "forward"}
	if len(args) != 2 || originEndpoint(args[0]) == originEndpoint(args[1]) {
		return op, usage("use forward origin:[PORT] PORT or forward PORT origin:[PORT]")
	}
	remote, local := args[1], args[0]
	if originEndpoint(args[1]) {
		remote, local, op.Reverse = args[0], args[1], true
	}
	op.LocalHint = originHint(local)
	if op.LocalHint != "" {
		if _, err := parsePort(op.LocalHint); err != nil {
			return op, err
		}
	}
	port, err := parsePort(remote)
	if op.Reverse {
		op.Port = port
	} else {
		op.TargetPort = port
	}
	return op, err
}

// Endpoint syntax requires brackets around IPv6; OpenSSH receives the bare address.
func endpointHost(host string) (string, error) {
	if err := validSSHDestination(host); err != nil {
		return "", err
	}
	user, address := "", host
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		user, address = host[:i+1], host[i+1:]
	}
	if strings.ContainsAny(address, "[]") {
		if len(address) < 2 || address[0] != '[' || address[len(address)-1] != ']' || net.ParseIP(address[1:len(address)-1]) == nil {
			return "", usage("invalid bracketed SSH destination")
		}
		address = address[1 : len(address)-1]
	} else if strings.Contains(address, ":") {
		return "", usage("IPv6 endpoints require brackets")
	}
	if address == "" {
		return "", usage("invalid SSH destination")
	}
	return user + address, nil
}
