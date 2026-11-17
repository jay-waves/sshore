package main

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Aliases resolve only command names, never operands or SSH arguments.
var commandAliases = map[string]string{
	"copy": "cp", "a": "attach", "d": "detach", "ps": "pstree",
	"ls": "list", "mnt": "mount", "fwd": "forward",
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
	cliPath
	cliDirectory
	cliDuration
	cliRetries
	cliPort
)

type cliOption struct {
	kind      cliValue
	alias     string
	conflicts []string
}

// Canonical option definitions own the public command grammar.
var cliOptions = map[string]cliOption{
	"--help":         {alias: "-h"},
	"--tldr":         {},
	"--version":      {alias: "-V"},
	"--local":        {conflicts: []string{"--server", "--download-dir", "--port"}},
	"--server":       {kind: cliText},
	"--port":         {kind: cliPort},
	"--download-dir": {kind: cliDirectory},
	"--quiet":        {alias: "-q"},
	"--no-escape":    {}, "--detach": {}, "--no-login": {}, "--no-profile": {},
	"--json": {},
	"--all":  {}, "--stay": {},
	"--foreground": {},
	"--read-only":  {}, "--stats": {}, "--direct-io": {},
	"--io-timeout": {kind: cliDuration}, "--startup-timeout": {kind: cliDuration},
	"--retries": {kind: cliRetries},
}

type cliSpec struct {
	flags       []string
	ssh, native bool
	min, max    int
}

func commandSpec(command string) (cliSpec, bool) {
	command = canonicalCommand(command)
	base := []string{"--local", "--server"}
	interactive := append(slices.Clone(base), "--download-dir", "--quiet", "--no-escape")
	spec := cliSpec{}
	switch command {
	case "":
		spec = cliSpec{[]string{"--version"}, false, false, 0, 0}
	case "mcp":
		spec = cliSpec{nil, false, false, 0, 0}
	case "exec":
		spec = cliSpec{nil, false, false, 2, 2}
	case "launch", "spawn":
		spec = cliSpec{append(interactive, "--detach", "--no-login", "--no-profile"), true, true, 0, 2}
	case "attach":
		spec = cliSpec{interactive, true, true, 0, 2}
	case "list":
		spec = cliSpec{append(base, "--json"), true, false, 0, 1}
	case "pstree":
		spec = cliSpec{nil, false, false, 0, 0}
	case "cancel":
		spec = cliSpec{append(base, "--all"), true, false, 0, 2}
	case "rename":
		spec = cliSpec{base, true, false, 2, 3}
	case "cp":
		spec = cliSpec{nil, true, false, 2, -1}
	case "mount":
		spec = cliSpec{strings.Fields("--server --foreground --read-only --io-timeout --startup-timeout --stats --direct-io"), true, false, 1, 2}
	case "forward":
		spec = cliSpec{strings.Fields("--retries --foreground"), true, false, 2, 2}
	case "review":
		spec = cliSpec{[]string{"--stay"}, false, false, 0, 1}
	case "help":
		spec = cliSpec{nil, false, false, 0, 1}
	case "detach", "reset":
	default:
		return spec, false
	}
	if spec.ssh {
		spec.flags = append(spec.flags, "--port")
	}
	spec.flags = append(spec.flags, "--help")
	if _, ok := commandExamples[command]; ok {
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
	operands []string
	values   map[string]string
	ssh      []string
}

func (a cliArguments) has(name string) bool { _, ok := a.values[name]; return ok }
func (a cliArguments) value(name, fallback string) string {
	if v, ok := a.values[name]; ok {
		return v
	}
	return fallback
}
func (a cliArguments) operand(index int, fallback string) string {
	if index < len(a.operands) {
		return a.operands[index]
	}
	return fallback
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
	}
	if name == "--server" && strings.ContainsAny(value, "\x00\r\n") {
		return usage("invalid server path")
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
	if spec.ssh {
		args, a.ssh, err = splitSSHOptions(args, spec.native)
		if err != nil {
			return a, err
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
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
	for _, name := range spec.flags {
		if !a.has(name) {
			continue
		}
		for _, other := range cliOptions[name].conflicts {
			if a.has(other) {
				return a, usage("%s and %s cannot be combined", name, other)
			}
		}
	}
	if a.has("--port") {
		flags := sshOptionKinds(spec.native)
		for i := 0; i < len(a.ssh); i++ {
			name := a.ssh[i]
			if flags[name] == cliBool {
				continue
			}
			i++
			if i < len(a.ssh) && sshPortSetting(name, a.ssh[i]) {
				return a, usage("--port cannot be combined with SSH -p or -o Port")
			}
		}
		a.ssh = append([]string{"-p", a.values["--port"]}, a.ssh...)
	}
	if a.has("--local") && len(a.ssh) != 0 {
		return a, usage("local shells do not accept SSH options")
	}
	max := spec.maxOperands(command, a)
	if len(a.operands) < spec.min {
		return a, usage("not enough arguments for %s; see sshore %s --help", command, command)
	}
	if max >= 0 && len(a.operands) > max {
		return a, usage("unexpected argument: %s", a.operands[max])
	}
	return a, nil
}

func (s cliSpec) maxOperands(command string, a cliArguments) int {
	max := s.max
	if a.has("--local") && slices.Contains([]string{"launch", "spawn", "attach", "cancel", "rename", "list", "pstree"}, command) {
		max--
	}
	if command == "cancel" && a.has("--all") {
		max = 1
		if a.has("--local") {
			max = 0
		}
	}
	return max
}

// One allowlist defines SSH options; only shell attachments permit
// native forwarding. SSH option repetition is intentional and retains order.
func sshOptionKinds(native bool) map[string]cliValue {
	result := map[string]cliValue{
		"-p": cliText, "-i": cliPath, "-J": cliText, "-F": cliPath,
		"-l": cliText, "-o": cliText, "-E": cliPath,
		"-C": cliBool, "-v": cliBool, "-vv": cliBool, "-vvv": cliBool, "-4": cliBool, "-6": cliBool,
	}
	if native {
		for _, name := range []string{"-L", "-R", "-D"} {
			result[name] = cliText
		}
	}
	return result
}

func sshOption(arg string, flags map[string]cliValue) (name, value string, kind cliValue, ok bool) {
	if kind, ok = flags[arg]; ok {
		return arg, "", kind, true
	}
	if len(arg) > 2 {
		name = arg[:2]
		kind, ok = flags[name]
		if ok && kind != cliBool {
			return name, arg[2:], kind, true
		}
	}
	return "", "", cliBool, false
}

func splitSSHOptions(args []string, nativeForwarding bool) ([]string, []string, error) {
	at := slices.Index(args, "--")
	if at < 0 {
		return args, nil, nil
	}
	suffix := args[at+1:]
	if len(suffix) == 0 {
		return nil, nil, usage("expected SSH options after --")
	}
	ssh, _, err := scanSSHOptions(suffix, nativeForwarding)
	return args[:at], ssh, err
}

func scanSSHOptions(args []string, native bool) ([]string, cliValue, error) {
	flags := sshOptionKinds(native)
	var ssh []string
	for i := 0; i < len(args); i++ {
		name, value, kind, ok := sshOption(args[i], flags)
		if !ok {
			return nil, cliBool, usage("unsupported SSH option after --: %s; put sshore arguments before --", args[i])
		}
		ssh = append(ssh, name)
		if kind == cliBool {
			continue
		}
		if value == "" {
			i++
			if i == len(args) {
				return nil, cliBool, usage("missing value for %s", name)
			}
			value = args[i]
		}
		if value == "" || value == "--" {
			return nil, cliBool, usage("missing value for %s", name)
		}
		ssh = append(ssh, value)
	}
	return ssh, cliBool, nil
}

func sshPortSetting(name, value string) bool {
	if name == "-p" {
		return true
	}
	if name != "-o" {
		return false
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
	return len(fields) > 0 && strings.EqualFold(fields[0], "Port")
}

func misplacedSSHOption(a string) error {
	if _, _, _, ok := sshOption(a, sshOptionKinds(true)); ok {
		return usage("SSH option %s must follow -- (example: sshore launch HOST -- -J jump)", a)
	}
	return nil
}
