package ssh

import "strings"

// SplitOption reads one supported command-line option. An empty name denotes
// an unrecognized token. An option missing its required value consumes
// zero arguments so callers can report a missing value.
func SplitOption(args []string) (name, value string, consumed int) {
	if len(args) == 0 {
		return "", "", 0
	}
	arg := args[0]
	switch arg {
	case "-C", "-v", "-vv", "-vvv", "-4", "-6":
		return arg, "", 1
	}
	if len(arg) >= 2 {
		switch arg[:2] {
		case "-p", "-i", "-J", "-F", "-l", "-o", "-c", "-m", "-L", "-R", "-D":
			if len(arg) > 2 {
				return arg[:2], arg[2:], 1
			}
			if len(args) > 1 && args[1] != "" && args[1] != "--" {
				return arg, args[1], 2
			}
			return arg, "", 0
		}
	}
	return "", "", 1
}

func ConfigKey(value string) string {
	value = strings.TrimLeft(value, " \t")
	if end := strings.IndexAny(value, "= \t"); end >= 0 {
		value = value[:end]
	}
	return strings.ToLower(value)
}

func ForwardingOption(name, value string) bool {
	return forwardingKey(name, value) != ""
}

func forwardingKey(name, value string) string {
	switch name {
	case "-L":
		return "localforward"
	case "-R":
		return "remoteforward"
	case "-D":
		return "dynamicforward"
	case "-o":
		key := ConfigKey(value)
		switch key {
		case "localforward", "remoteforward", "dynamicforward":
			return key
		}
	}
	return ""
}

func PortOption(name, value string) bool {
	return name == "-p" || name == "-o" && ConfigKey(value) == "port"
}

// WithoutForwarding retains options in their original form and order.
func WithoutForwarding(args []string) []string {
	return appendFilteredOptions(nil, args, ForwardingOption)
}

func diagnosticOption(name, value string) bool {
	return name == "-v" || name == "-vv" || name == "-vvv" || name == "-o" && ConfigKey(value) == "loglevel"
}

// RouteArgs retains connection settings for saving and reusing an SSH route.
func RouteArgs(args []string) []string {
	return appendFilteredOptions(nil, args, func(name, value string) bool {
		return diagnosticOption(name, value) || ForwardingOption(name, value)
	})
}

// WithoutDiagnostics keeps options that can affect how a connection is established.
func WithoutDiagnostics(args []string) []string {
	return appendFilteredOptions(nil, args, diagnosticOption)
}

func DiagnosticOptions(args []string) []string {
	return appendFilteredOptions(nil, args, func(name, value string) bool { return !diagnosticOption(name, value) })
}

// AppendWithoutPort keeps the destination's explicit port and appends the
// remaining options in their original form and order.
func AppendWithoutPort(dst, args []string) []string {
	return appendFilteredOptions(dst, args, PortOption)
}

func appendFilteredOptions(dst, args []string, exclude func(string, string) bool) []string {
	for len(args) > 0 {
		name, value, n := SplitOption(args)
		n = max(n, 1)
		if !exclude(name, value) {
			dst = append(dst, args[:n]...)
		}
		args = args[n:]
	}
	return dst
}
