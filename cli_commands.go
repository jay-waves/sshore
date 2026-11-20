package main

import (
	"context"
	"net"
	"os"
	"strings"
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
		return o, usage("choose an action: sshore launch [HOST:], sshore spawn [HOST:], sshore list HOST:, sshore attach HOST:ID, sshore kill HOST:ID, or sshore rename HOST:ID NEW")
	}
	parsed, err := parseCLI(args[1:], o.command)
	if err != nil {
		return o, err
	}
	return shellOptions(o.command, parsed)
}

func shellOptions(command string, parsed cliArguments) (options, error) {
	o := options{command: command, list: command == "list", server: parsed.value("--server", "sshore")}
	o.ssh, o.local = parsed.ssh, parsed.local
	o.portOverrideOnly = parsed.has("--port") && len(parsed.ssh) == 2
	o.serverSet = parsed.has("--server")
	o.quiet, o.noEscape = parsed.has("--quiet"), parsed.has("--no-escape")
	o.detached = parsed.has("--detach")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	o.listJSON = parsed.has("--json")
	firstName := 1
	o.host = parsed.operand(0)
	if o.local {
		o.host, firstName = "local", 0
		if len(o.ssh) > 0 || o.serverSet {
			return o, usage("local shells do not accept SSH options")
		}
	}
	if !o.list {
		o.session = parsed.operand(firstName)
	}
	if command == "rename" {
		o.newName = parsed.operand(firstName + 1)
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
			return o, usage("rename requires HOST:ID and a new alias")
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
	return o, validSession(o.session)
}

// Host-only commands accept the legacy SSH destination as well as HOST:.
func normalizeHostOperand(command string, parsed *cliArguments) error {
	ref := parsed.operands[0]
	if !strings.Contains(ref, ":") {
		return validSSHDestination(ref)
	}
	// Keep legacy unbracketed IPv6 destinations. Endpoints use [IPv6]:.
	_, address, hasUser := strings.Cut(ref, "@")
	if !hasUser {
		address = ref
	}
	if ref != "::" && net.ParseIP(address) != nil {
		return validSSHDestination(ref)
	}
	if strings.HasSuffix(address, "]") {
		if _, err := endpointHost(ref); err == nil {
			return nil
		}
	}
	host, value, err := splitEndpoint(ref)
	if err != nil {
		return err
	}
	if value != "" {
		return usage("%s requires an empty value after the host colon", command)
	}
	switch host {
	case "origin":
		return usage("%s requires here: or an SSH host", command)
	case "here":
		if command == "exec" {
			return usage("exec requires an SSH host")
		}
		if len(parsed.ssh) != 0 || parsed.has("--server") {
			return usage("local tasks do not accept SSH options")
		}
		parsed.local = true
		parsed.operands = nil
	default:
		parsed.operands[0] = host
	}
	return nil
}

func normalizeShellSelector(command string, parsed *cliArguments) error {
	count := len(parsed.operands)
	if command == "launch" || command == "spawn" {
		host := "here"
		if count != 0 {
			endpoint, suffix, err := splitEndpoint(parsed.operands[0])
			if err != nil {
				return err
			}
			if suffix != "" {
				return usage("create with %s HOST: and set an alias with rename", command)
			}
			if endpoint == "origin" {
				return usage("create shells on here: or an SSH host")
			}
			host = endpoint
		}
		if host == "here" {
			parsed.local = true
			parsed.operands = nil
		} else {
			parsed.operands[0] = host
		}
		return nil
	}
	if parsed.has("--all") || !((command == "attach" || command == "kill") && count == 1 || command == "rename" && count == 2) {
		return nil
	}
	ref := parsed.operands[0]
	if !strings.Contains(ref, ":") || strings.Contains(ref, "/") {
		return nil
	}
	host, selector, err := splitEndpoint(ref)
	if err != nil {
		return err
	}
	if selector == "" && command != "attach" {
		return usage("%s requires a task ID or alias after the colon", command)
	}
	if selector != "" {
		if err := validSession(selector); err != nil {
			return err
		}
	}
	tail := parsed.operands[1:]
	parsed.explicitHost = true
	if host == "origin" {
		return usage("shell selectors require here: or an SSH host")
	}
	if host == "here" {
		if len(parsed.ssh) != 0 || parsed.has("--server") {
			return usage("local shells do not accept SSH options")
		}
		parsed.local = true
		if selector == "" {
			parsed.operands = tail
		} else {
			parsed.operands = append([]string{selector}, tail...)
		}
		return nil
	}
	operands := []string{host}
	if selector != "" {
		operands = append(operands, selector)
	}
	parsed.operands = append(operands, tail...)
	return nil
}

type options struct {
	handoffHint           *handoffObservation
	recovering            bool
	reusable              *clientTransport
	routeTarget           bool
	handoffDone           func(context.Context, error) error
	routeParent           *options
	routeToken            string
	environment           []string
	tree                  []shellTreeNode
	temporary             bool
	hosted                bool
	owner                 processIdentity
	host, session, server string
	command               string
	newName               string
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
	if o.command == "attach" && o.session == "" && o.taskIdentity == "" {
		if err := resolveDetachedAttach(&o); err != nil {
			return 1, err
		}
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

const localEndpointPrefix = "here:"

func splitEndpoint(value string) (host, suffix string, err error) {
	switch {
	case strings.HasPrefix(value, "::"):
		return "origin", value[2:], nil
	case strings.HasPrefix(value, ".:"):
		return "here", value[2:], nil
	case strings.HasPrefix(value, ":"):
		return "here", value[1:], nil
	}
	address, offset := value, 0
	if user, after, ok := strings.Cut(value, "@"); ok && !strings.ContainsAny(user, ":/\\") {
		address, offset = after, len(user)+1
	}
	start := 0
	if strings.HasPrefix(address, "[") {
		end := strings.IndexByte(address, ']')
		if end < 0 || end+1 == len(address) || address[end+1] != ':' {
			return "", "", usage("use [HOST]:VALUE for an IPv6 endpoint")
		}
		start = end + 1
	} else if ip := net.ParseIP(address); ip != nil && strings.Contains(address, ":") {
		return "", "", usage("IPv6 endpoints require brackets")
	}
	separator := strings.IndexByte(address[start:], ':')
	if separator < 0 {
		return "", "", usage("endpoint requires a colon: use HOST:VALUE, here:VALUE or origin:VALUE")
	}
	separator += offset + start
	if strings.ContainsAny(value[:separator], "/\\") {
		return "", "", usage("invalid endpoint host")
	}
	host, err = endpointHost(value[:separator])
	return host, value[separator+1:], err
}

func remoteCopyRequest(args []string) (string, []string, string, error) {
	fail := func() (string, []string, string, error) {
		return "", nil, "", usage("use cp here:SOURCE... origin:[PATH] or cp origin:[PATH] here:DIRECTORY")
	}
	if len(args) < 2 {
		return fail()
	}
	first, firstPath, err := splitEndpoint(args[0])
	if err != nil {
		return "", nil, "", err
	}
	last, lastPath, err := splitEndpoint(args[len(args)-1])
	if err != nil {
		return "", nil, "", err
	}
	kind, sources, hint := "download", args[:len(args)-1], lastPath
	switch {
	case first == "origin" && last == "here" && len(args) == 2:
		kind, sources, hint = "upload", args[1:], firstPath
	case first == "here" && last == "origin":
	default:
		return fail()
	}
	if len(hint) > 4096 || strings.ContainsAny(hint, "\x00\r\n") {
		return fail()
	}
	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		host, path, err := splitEndpoint(source)
		if err != nil || host != "here" || path == "-" || (path == "" && kind != "upload") {
			return fail()
		}
		if path == "" {
			path = "."
		}
		paths = append(paths, path)
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
	sourceHost, source, err := splitEndpoint(parsed.operands[0])
	if err != nil {
		return operation{}, err
	}
	targetHost, hint, err := splitEndpoint(parsed.operands[1])
	if err != nil {
		return operation{}, err
	}
	if sourceHost != "here" || targetHost != "origin" {
		return operation{}, usage("use mount here:[DIRECTORY] origin:[MOUNTPOINT]")
	}
	if source == "" {
		source = "."
	}
	if len(parsed.ssh) != 0 {
		return operation{}, usage("remote mount requests inherit the attached connection")
	}
	for name := range parsed.values {
		if name != "--read-only" {
			return operation{}, usage("remote mount requests accept only --read-only")
		}
	}
	return operation{Kind: "mount", Path: source, LocalHint: hint, ReadOnly: parsed.has("--read-only")}, nil
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
	host, path, err := splitEndpoint(value)
	if err != nil {
		return copyEndpoint{}, err
	}
	switch host {
	case "here":
		return copyEndpoint{path: path, pick: path == "", stream: path == "-"}, nil
	case "origin":
		return copyEndpoint{}, usage("origin: requires an attached sshore shell")
	}
	if path == "" {
		path = "."
	}
	return copyEndpoint{host: host, path: path}, nil
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

func parseFTPOptions(args []string) (ftpOptions, error) {
	parsed, err := parseCLI(args, "mount")
	if err != nil {
		return ftpOptions{}, err
	}
	host, remote, err := splitEndpoint(parsed.operands[0])
	if err != nil {
		return ftpOptions{}, err
	}
	targetHost, mountpoint, err := splitEndpoint(parsed.operands[1])
	if err != nil {
		return ftpOptions{}, err
	}
	if host == "here" || host == "origin" || targetHost != "here" {
		return ftpOptions{}, usage("use mount HOST:SOURCE here:[MOUNTPOINT]")
	}
	if remote == "" {
		remote = "."
	}
	o := ftpOptions{
		Host: host, Remote: remote, Mountpoint: mountpoint,
		SSH:        parsed.ssh,
		foreground: parsed.has("--foreground"),
		ReadOnly:   parsed.has("--read-only"), Stats: parsed.has("--stats"), DirectIO: parsed.has("--direct-io"),
		IOTimeout: parsed.duration("--io-timeout", defaultMountIOTimeout), StartupTimeout: parsed.duration("--startup-timeout", 0),
	}
	if strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}
