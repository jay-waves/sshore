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
		case "-p", "-i", "-J", "-F", "-l", "-o", "-E", "-L", "-R", "-D":
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
	switch name {
	case "-L", "-R", "-D":
		return true
	case "-o":
		switch ConfigKey(value) {
		case "localforward", "remoteforward", "dynamicforward":
			return true
		}
	}
	return false
}

func PortOption(name, value string) bool {
	return name == "-p" || name == "-o" && ConfigKey(value) == "port"
}

// WithoutForwarding retains options in their original form and order.
func WithoutForwarding(args []string) []string {
	return appendFilteredOptions(nil, args, ForwardingOption)
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
