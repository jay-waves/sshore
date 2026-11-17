package main

import (
	"context"
	"fmt"
	"github.com/jay-waves/sshore/internal/files"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected launch, spawn, list, attach, kill or rename; see sshore --help")
	}
	o.command = canonicalCommand(args[0])
	switch o.command {
	case "launch", "spawn", "attach", "kill", "rename":
	case "list":
		o.list = true
	default:
		return o, usage("choose an action: sshore launch HOST [NAME], sshore spawn HOST [NAME], sshore list HOST, sshore attach HOST [NAME], sshore kill HOST NAME, or sshore rename HOST OLD NEW")
	}
	parsed, err := parseCLI(args[1:], o.command)
	if err != nil {
		return o, err
	}
	return shellOptions(o.command, parsed)
}

func shellOptions(command string, parsed cliArguments) (options, error) {
	o := options{command: command, list: command == "list", server: parsed.value("--server", "sshore")}
	o.ssh, o.local = parsed.ssh, parsed.has("--local")
	o.portOverrideOnly = parsed.has("--port") && len(parsed.ssh) == 2
	o.serverSet = parsed.has("--server")
	o.downloadDir = parsed.values["--download-dir"]
	o.quiet, o.noEscape = parsed.has("--quiet"), parsed.has("--no-escape")
	o.detached = parsed.has("--detach")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	o.listJSON = parsed.has("--json")
	if (o.command == "launch" || o.command == "spawn") && len(parsed.operands) == 0 {
		o.local = true
	}
	firstName := 1
	o.host = parsed.operand(0, "")
	if o.local {
		o.host, firstName = "local", 0
		if len(o.ssh) > 0 || o.serverSet || o.downloadDir != "" {
			return o, usage("local shells do not accept SSH or transfer options")
		}
	}
	if !o.list {
		o.session = parsed.operand(firstName, "")
	}
	if command == "rename" {
		o.newName = parsed.operand(firstName+1, "")
	}

	if o.host == "" && o.list && len(o.ssh) == 0 && o.server == "sshore" {
		return o, nil
	}
	if o.host != "" || o.command != "attach" {
		if err := validSSHDestination(o.host); err != nil {
			return o, err
		}
	}
	if o.list {
		return o, nil
	}
	if o.command == "rename" {
		if o.session == "" || o.newName == "" {
			return o, usage("rename requires HOST OLD NEW and accepts SSH options, --server and --port")
		}
		if err := validAlias(o.newName); err != nil {
			return o, err
		}
	}
	if o.command == "kill" && (o.session == "") {
		return o, usage("kill requires HOST NAME and accepts SSH options, --server and --port")
	}
	if o.session == "" {
		return o, nil
	}
	if o.command == "launch" || o.command == "spawn" {
		return o, validAlias(o.session)
	}
	return o, validSession(o.session)
}

type options struct {
	transports            *clientTransports
	reusable              *clientTransport
	routeTarget           bool
	handoffDone           func(context.Context, error) error
	routeParent           *options
	routeToken            string
	daemonTask            *localShellRecord
	environment           []string
	tree                  []shellTreeNode
	temporary             bool
	hosted                bool
	owner                 spawnOwner
	host, session, server string
	command               string
	newName               string
	downloadDir           string
	list, quiet, noEscape bool
	listJSON              bool
	ssh                   []string
	shellPID              int
	local                 bool
	detached              bool
	noLogin               bool
	noProfile             bool
	taskIdentity          string
	instance              string
	portOverrideOnly      bool
	serverSet             bool
	sshDirectory          string
}

func executeShellCommand(o options) (int, error) {
	var err error
	if o.command == "spawn" {
		o.temporary = true
		o.owner, err = currentSpawnOwner()
		if err != nil {
			return 1, err
		}
	}
	if handled, err := handoffRemoteSpawn(o); handled {
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	if o.local {
		if err := resolveLocalAttach(&o); err != nil {
			return 1, err
		}
		if handled, err := handoffLocalClient(o); handled {
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
		return runLocalCommand(o)
	}
	if o.taskIdentity == "" {
		if err = resolveRecentAttach(&o); err != nil {
			return 1, err
		}
	}
	o.downloadDir, err = prepareDownloadDir(o.downloadDir)
	if err != nil {
		return 1, err
	}
	if o.list {
		return runInventory(o)
	}
	if handled, err := handoffLocalClient(o); handled {
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	return runClient(o)
}

func prepareDownloadDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("download destination is not a directory: %s", path)
	}
	return path, nil
}

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
	host, path, err := splitRemotePath(value)
	if err != nil {
		return copyEndpoint{}, err
	}
	if host != "" {
		return copyEndpoint{host: host, path: path}, nil
	}
	if value == "" {
		return copyEndpoint{}, usage("empty copy path")
	}
	return copyEndpoint{path: value}, nil
}

// splitRemotePath recognizes HOST:PATH without interpreting local endpoint roles.
func splitRemotePath(value string) (host, path string, err error) {
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
				return "", "", err
			}
			remote := value[i+1:]
			if remote == "" {
				remote = "."
			}
			return host, remote, nil
		}
	}
	return "", "", nil
}

// remotePathOperand encodes a normalized SSH destination for a generated command.
func remotePathOperand(host, path string) string {
	prefix, addr := "", host
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		prefix, addr = host[:i+1], host[i+1:]
	}
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		addr = "[" + addr + "]"
	}
	return prefix + addr + ":" + path
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
	if len(o.sources) > maxRequestItems {
		return o, usage("at most %d files per copy", maxRequestItems)
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
			return ftpOptions{}, usage("origin: mount requests require an attached sshore shell; run mount HOST:SOURCE locally")
		}
	}
	source := parsed.operands[0]
	address := source
	if user, rest, ok := strings.Cut(source, "@"); ok && !strings.ContainsAny(user, ":/\\") {
		address = rest
	}
	if strings.Contains(address, ":") && net.ParseIP(address) != nil {
		return ftpOptions{}, usage("IPv6 mount sources require brackets: [HOST]:SOURCE")
	}
	host, remote, err := splitRemotePath(source)
	if err != nil {
		return ftpOptions{}, err
	}
	if host == "" {
		return ftpOptions{}, usage("use mount HOST:SOURCE [MOUNTPOINT]; use HOST: for the remote home")
	}
	o := ftpOptions{
		host: host, remote: remote, mountpoint: parsed.operand(1, ""),
		ssh: parsed.ssh, server: parsed.value("--server", "sshore"),
		foreground: parsed.has("--foreground"),
		readOnly:   parsed.has("--read-only"), stats: parsed.has("--stats"), directIO: parsed.has("--direct-io"),
		ioTimeout: parsed.duration("--io-timeout", 60*time.Second), startupTimeout: parsed.duration("--startup-timeout", 0),
	}
	if strings.ContainsRune(o.remote, 0) || strings.ContainsRune(o.mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}
