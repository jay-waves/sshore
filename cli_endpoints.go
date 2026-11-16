package main

import (
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
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

func remoteMountRequest(args []string) (operation, error) {
	parsed, err := parseCLI(args, "mount")
	if err != nil {
		return operation{}, err
	}
	if len(parsed.operands) != 2 || originEndpoint(parsed.operands[0]) || !originEndpoint(parsed.operands[1]) {
		return operation{}, usage("use mount DIRECTORY origin:[MOUNTPOINT] [--read-only]")
	}
	if len(parsed.ssh) != 0 {
		return operation{}, usage("remote mount requests inherit the attached connection")
	}
	for name := range parsed.values {
		if name != "--read-only" {
			return operation{}, usage("remote mount requests accept only --read-only")
		}
	}
	endpoint, err := parseCopyEndpoint(parsed.operands[0])
	if err != nil || endpoint.host != "" || endpoint.stream {
		return operation{}, usage("mount source must be a path on this server")
	}
	return operation{Kind: "mount", Path: parsed.operands[0], LocalHint: originHint(parsed.operands[1]), ReadOnly: parsed.has("--read-only")}, nil
}

// validSSHDestination applies to SSH aliases as well as user@host addresses.
func validSSHDestination(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.IndexFunc(host, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
		return usage("invalid SSH destination")
	}
	return nil
}

func sshExecutable() string {
	if exe := os.Getenv("SSHORE_SSH"); exe != "" {
		return exe
	}
	return "ssh"
}

func parsePort(s string) (int, error) {
	n, err := parseForwardPort(s)
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
	parsed, err := parseCLI(args, "forward")
	if err != nil {
		return f, false, err
	}
	f.SSH, f.Name = parsed.ssh, parsed.values["--name"]
	f.Retries = parsed.integer("--retries")
	foreground := parsed.has("--foreground")
	if foreground && f.Retries != 0 {
		return f, false, usage("--retries applies only to background forwards")
	}
	f.Host, f.Port, f.Target, f.Reverse, err = parseForwardEndpoints(parsed.operands)
	return f, foreground, err
}

type copyEndpoint struct {
	host, path   string
	pick, stream bool
}
type copyOptions struct {
	sources []copyEndpoint
	target  copyEndpoint
	host    string
	upload  bool
	ssh     []string
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	if value == "-" {
		return copyEndpoint{stream: true}, nil
	}
	if originPlaceholder(value) {
		return copyEndpoint{pick: true}, nil
	}
	if strings.HasPrefix(value, "origin:") {
		return copyEndpoint{path: strings.TrimPrefix(value, "origin:")}, nil
	}
	// Windows drive prefixes are local only on Windows. On Linux, a single
	// letter can be a valid SSH host alias.
	if runtime.GOOS == "windows" && len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) {
		return copyEndpoint{path: value}, nil
	}
	bracket := false
	for i, c := range value {
		if c == '[' {
			bracket = true
		}
		if c == ']' {
			bracket = false
		}
		if !bracket && (c == '/' || c == '\\') {
			break
		}
		if c == ':' && !bracket {
			host, err := endpointHost(value[:i])
			if err != nil {
				return copyEndpoint{}, err
			}
			remote := value[i+1:]
			if remote == "" {
				remote = "."
			}
			return copyEndpoint{host: host, path: remote}, nil
		}
	}
	if value == "" {
		return copyEndpoint{}, usage("empty copy path")
	}
	return copyEndpoint{path: value}, nil
}

func parseCopyCommand(args []string) (copyOptions, error) {
	var o copyOptions
	parsed, err := parseCLI(args, "cp")
	if err != nil {
		return o, err
	}
	args, o.ssh = parsed.operands, parsed.ssh
	var endpoints []copyEndpoint
	for _, a := range args {
		e, err := parseCopyEndpoint(a)
		if err != nil {
			return o, err
		}
		endpoints = append(endpoints, e)
	}
	o.sources, o.target = endpoints[:len(endpoints)-1], endpoints[len(endpoints)-1]
	o.upload = o.target.host != ""
	if o.upload {
		o.host = o.target.host
	}
	for _, source := range o.sources {
		if (source.pick || source.stream || o.target.stream) && len(o.sources) != 1 {
			return o, usage("selection and standard streams require exactly one source")
		}
		if o.upload {
			if source.host != "" {
				return o, usage("remote-to-remote copies are not supported")
			}
		} else {
			if source.host == "" {
				return o, usage("cp requires a remote source or destination")
			}
			if o.host != "" && o.host != source.host {
				return o, usage("all sources must use the same SSH destination")
			}
			o.host = source.host
		}
	}
	if len(o.sources) > maxApprovalItems {
		return o, usage("at most %d files per copy", maxApprovalItems)
	}
	return o, nil
}

type ftpOptions struct {
	approval                  *files.MountApproval
	ioTimeout, startupTimeout time.Duration
	host, remote, mountpoint  string
	ssh                       []string
	readOnly                  bool
	directIO                  bool
	foreground                bool
	stats                     bool
	noWatch                   bool
	server                    string
	name                      string
}

func parseFTPOptions(args []string) (ftpOptions, error) {
	parsed, err := parseCLI(args, "mount")
	if err != nil {
		return ftpOptions{}, err
	}
	for _, value := range parsed.operands {
		if originEndpoint(value) {
			return ftpOptions{}, usage("origin: mount requests require an attached sshore shell; run mount HOST DIRECTORY locally")
		}
	}
	o := ftpOptions{
		host: parsed.operand(0, ""), remote: parsed.operand(1, "."), mountpoint: parsed.operand(2, ""),
		ssh: parsed.ssh, name: parsed.values["--name"], server: parsed.value("--server", "sshore"),
		foreground: parsed.has("--foreground"), noWatch: parsed.has("--no-watch"),
		readOnly: parsed.has("--read-only"), stats: parsed.has("--stats"), directIO: parsed.has("--direct-io"),
		ioTimeout: parsed.duration("--io-timeout", 60*time.Second), startupTimeout: parsed.duration("--startup-timeout", 0),
	}
	if err := validSSHDestination(o.host); err != nil {
		return o, err
	}
	if strings.ContainsRune(o.remote, 0) || strings.ContainsRune(o.mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}
