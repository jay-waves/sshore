package main

import (
	"runtime"
	"strconv"
	"strings"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

// Aliases resolve only command names, never operands or SSH arguments.
var commandAliases = map[string]string{
	"copy": "cp", "a": "attach", "d": "detach", "ps": "pstree",
	"ls": "list", "mnt": "mount", "fwd": "forward",
	"shell": "sh",
}

func canonicalCommand(name string) string {
	if command, ok := commandAliases[name]; ok {
		return command
	}
	return name
}

type cliValue uint8

const (
	cliBool cliValue = iota
	cliText
	cliDuration
	cliRetries
	cliPort
	cliPingCount
)

type cliOption struct {
	kind  cliValue
	alias string
}

// Canonical option definitions own the public command grammar.
var cliOptions = map[string]cliOption{
	"--help":      {alias: "-h"},
	"--tldr":      {},
	"--version":   {alias: "-V"},
	"--server":    {kind: cliText},
	"--apply":     {},
	"--port":      {kind: cliPort},
	"--no-escape": {}, "--pin": {}, "--no-login": {}, "--no-profile": {},
	"--json":       {},
	"--all":        {},
	"--foreground": {},
	"--read-only":  {}, "--stats": {}, "--direct-io": {},
	"--io-timeout": {kind: cliDuration}, "--startup-timeout": {kind: cliDuration},
	"--retries": {kind: cliRetries},
	"--count":   {kind: cliPingCount},
}

type cliSpec struct {
	flags    []string
	ssh      bool
	min, max int
	help     string
	examples string
}

func commandSpec(command string) (cliSpec, bool) {
	spec, ok := cliCommands[command]
	if !ok {
		return cliSpec{}, false
	}
	spec.flags = append(make([]string, 0, len(spec.flags)+3), spec.flags...)
	if spec.ssh {
		spec.flags = append(spec.flags, "--port")
	}
	spec.flags = append(spec.flags, "--help")
	if spec.examples != "" {
		spec.flags = append(spec.flags, "--tldr")
	}
	return spec, true
}

func (s cliSpec) option(name string) (string, cliOption, bool) {
	for _, canonical := range s.flags {
		option := cliOptions[canonical]
		if name == canonical || name == option.alias {
			return canonical, option, true
		}
	}
	return "", cliOption{}, false
}

type cliArguments struct {
	taskKind     string
	bareAttachID bool
	local        bool
	operands     []string
	values       map[string]string
	ssh          []string
}

func (a cliArguments) has(name string) bool { _, ok := a.values[name]; return ok }
func (a cliArguments) value(name, fallback string) string {
	if v, ok := a.values[name]; ok {
		return v
	}
	return fallback
}
func (a cliArguments) operand(index int) string {
	if index < len(a.operands) {
		return a.operands[index]
	}
	return ""
}
func (a cliArguments) duration(name string, fallback time.Duration) time.Duration {
	if !a.has(name) {
		return fallback
	}
	value, _ := time.ParseDuration(a.values[name])
	return value
}
func (a cliArguments) integer(name string) int {
	value, _ := strconv.Atoi(a.values[name])
	return value
}

func validateCLIValue(name, value string, option cliOption) error {
	switch option.kind {
	case cliDuration:
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			return usage("%s requires a nonnegative duration", name)
		}
	case cliPort:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 65535 || strings.Trim(value, "0123456789") != "" {
			return usage("%s requires a port from 1 to 65535", name)
		}
	case cliRetries:
		if _, err := parseForwardRetries(value); err != nil {
			return usage("%s: %s", name, err)
		}
	case cliPingCount:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 1000 || strings.Trim(value, "0123456789") != "" {
			return usage("%s requires a count from 1 to 1000", name)
		}
	}
	if name == "--server" && strings.ContainsAny(value, "\x00\r\n") {
		return usage("invalid %s value", name)
	}
	if name == "--direct-io" && runtime.GOOS != "linux" {
		return usage("--direct-io is supported only on Linux")
	}
	return nil
}

// The suffix after -- passes SSH options; --port supplies the SSH port.
// Product aliases share one slot; repeated SSH options retain their order.
// Parsing never starts work or reads state.
func parseCLI(args []string, command string) (cliArguments, error) {
	command = canonicalCommand(command)
	a := cliArguments{values: make(map[string]string)}
	spec, ok := commandSpec(command)
	if !ok {
		return a, usage("unknown command: %s", command)
	}
	var err error
	hasSSHPort := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if spec.ssh && arg == "--" && (command != "exec" || len(a.operands) == 2) {
			if i+1 == len(args) {
				return a, usage("expected SSH options after --")
			}
			var suffix []string
			suffix, hasSSHPort, err = parseSSHOptions(args[i+1:])
			if err != nil {
				return a, err
			}
			a.ssh = append(a.ssh, suffix...)
			break
		}
		if arg == "" {
			return a, usage("empty argument")
		}
		if command == "exec" && len(a.operands) > 0 {
			a.operands = append(a.operands, arg)
			continue
		}
		if arg == "--tldr" {
			return a, usage("use sshore %s --tldr without other arguments", command)
		}
		if arg == "--help" || arg == "-h" {
			return a, errHelp
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			a.operands = append(a.operands, arg)
			continue
		}
		if name, value, count := sshio.SplitOption(args[i:]); spec.ssh && sshio.ForwardingOption(name, value) {
			if count == 0 {
				return a, usage("missing value for %s", name)
			}
			a.ssh = append(a.ssh, name, value)
			i += count - 1
			continue
		}
		if err := misplacedSSHOption(arg); err != nil {
			return a, err
		}
		name, value, attached := strings.Cut(arg, "=")
		canonical, option, found := spec.option(name)
		if !found {
			return a, usage("unsupported option: %s", name)
		}
		if a.has(canonical) {
			return a, usage("%s may only be specified once", canonical)
		}
		if option.kind != cliBool {
			if !attached {
				i++
				if i == len(args) || strings.HasPrefix(args[i], "--") {
					return a, usage("missing value for %s", name)
				}
				value = args[i]
			}
			if value == "" {
				return a, usage("missing value for %s", name)
			}
		} else if attached {
			return a, usage("%s does not accept a value", name)
		}
		a.values[canonical] = value
		if err := validateCLIValue(canonical, value, option); err != nil {
			return a, err
		}
	}
	if a.has("--port") {
		if hasSSHPort {
			return a, usage("--port cannot be combined with SSH -p or -o Port")
		}
		a.ssh = append([]string{"-p", a.values["--port"]}, a.ssh...)
	}
	max := spec.maxOperands(command, a)
	if len(a.operands) < spec.min {
		return a, usage("not enough arguments for %s; see sshore %s --help", command, command)
	}
	if max >= 0 && len(a.operands) > max {
		return a, usage("unexpected argument: %s", a.operands[max])
	}
	if len(a.operands) != 0 && (command == "exec" || command == "ping" || command == "copy-id" || command == "kill" && a.has("--all")) {
		err = normalizeHostOperand(command, &a)
	} else {
		err = normalizeShellSelector(command, &a)
	}
	return a, err
}

func (s cliSpec) maxOperands(command string, a cliArguments) int {
	max := s.max
	if command == "kill" && a.has("--all") {
		max = 1
	}
	return max
}

func parseSSHOptions(suffix []string) ([]string, bool, error) {
	var ssh []string
	hasPort := false
	for len(suffix) > 0 {
		name, value, n := sshio.SplitOption(suffix)
		if name == "" {
			return nil, false, usage("unsupported SSH option after --: %s; put sshore arguments before --", suffix[0])
		}
		if n == 0 || value == "--" {
			return nil, false, usage("missing value for %s", name)
		}
		hasPort = hasPort || sshio.PortOption(name, value)
		ssh = append(ssh, name)
		if value != "" {
			ssh = append(ssh, value)
		}
		suffix = suffix[n:]
	}
	return ssh, hasPort, nil
}

func misplacedSSHOption(a string) error {
	if name, _, _ := sshio.SplitOption([]string{a}); name != "" {
		return usage("SSH option %s must follow -- (example: sshore sh HOST: -- -J jump)", a)
	}
	return nil
}
