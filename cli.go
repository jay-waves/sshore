package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func run(args []string) (code int, resultErr error) {
	defer func() {
		if errors.Is(resultErr, errHelp) && len(args) > 0 {
			code, resultErr = 0, showHelp(args[:1], os.Stdout)
		}
	}()
	if len(args) > 0 {
		args = append([]string(nil), args...)
		args[0] = canonicalCommand(args[0])
		if cliCommands[args[0]].help != "" {
			if len(args) == 2 && args[1] == "--tldr" {
				return 0, showTLDR(args[0], os.Stdout)
			}
			for i, arg := range args[1:] {
				if args[0] == "exec" && i > 0 {
					break
				}
				if arg == "--" {
					break
				}
				if arg == "--help" || arg == "-h" {
					return 0, showHelp(args[:1], os.Stdout)
				}
			}
		}
	}
	if os.Getenv("SSHORE_DISABLED") == "1" && len(args) > 0 {
		allowed := true
		switch args[0] {
		case "--version", "-V", "--help", "-h", "--tldr", "help", remoteServiceCommand, "pstree", "reset", "list":
		default:
			allowed = false
		}
		if !allowed {
			return 1, disabledFeaturesError()
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}

	switch args[0] {
	case "ping":
		return runPing(args[1:])
	case "exec":
		return runExec(args[1:])
	case "reset":
		if len(args) != 1 {
			return 2, usage("reset takes no arguments")
		}
		return 0, runReset()
	case "--tldr":
		if len(args) != 1 {
			return 2, usage("usage: sshore --tldr or sshore COMMAND --tldr")
		}
		return 0, showTLDR("", os.Stdout)
	case "help":
		return 0, showHelp(args[1:], os.Stdout)
	case "-h", "--help":
		return 0, showHelp(nil, os.Stdout)
	case "-V", "--version":
		if len(args) != 1 {
			return 2, usage("--version takes no arguments")
		}
		fmt.Println("sshore", version)
		return 0, nil
	case "install":
		return runInstall(args[1:])
	case remoteServiceCommand:
		return 0, runRemoteService(args[1:])
	case "daemon":
		parsed, err := parseCLI(args[1:], "daemon")
		if err != nil {
			return 2, err
		}
		if len(parsed.operands) == 0 {
			return 0, runDaemon()
		}
		if parsed.operands[0] != "mount" {
			return 2, usage("usage: sshore daemon [mount]")
		}
		return runMountDaemon()
	case "launch", "spawn", "attach", "rename", "kill":
		return runTargetCommand(args)
	case "list", "pstree":
		return runInventoryCommand(args[0], args[1:])
	case "mount":
		return runMount(args[1:])
	case "forward":
		return runEndpointForward(args[1:])
	case "cp":
		return runCopy(args[1:])
	case "copy-id":
		return runCopyID(args[1:])
	case "detach":
		if len(args) != 1 {
			return 2, usage("detach takes no arguments")
		}
		if identity := os.Getenv("SSHORE_LOCAL_SESSION"); identity != "" {
			return 0, detachLocalShell(identity)
		}
		return 0, detachHelper()
	}
	return 2, usage("unknown command: %s", args[0])
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

var errHelp = errors.New("help requested")

func killAllOptions(parsed cliArguments) (options, error) {
	o := options{host: parsed.operand(0), ssh: parsed.ssh, server: parsed.value("--server", "sshore")}
	if o.host == "" && (len(o.ssh) != 0 || o.server != "sshore") {
		return o, validSSHDestination(o.host)
	}
	return o, nil
}

// Route task selectors before command semantics; no execution starts while
// selecting a route. Ordinary shell and kill forms share one parsed result.
func runTargetCommand(args []string) (int, error) {
	command := args[0]
	parsed, err := parseCLI(args[1:], command)
	if err != nil {
		return 2, err
	}
	target, selected, err := resolveShortTask(command, &parsed)
	if err != nil {
		return 2, err
	}
	if selected && target.Kind != "shell" {
		switch command {
		case "kill":
			return withDaemonActivity(func() (int, error) { return 0, stopResource(target.Kind, target.Name, target.Identity) })
		case "rename":
			return withDaemonActivity(func() (int, error) {
				return runRenameResource(target.Kind, target.Name, parsed.operands[1], target.Identity)
			})
		}
	}
	if command == "kill" && parsed.has("--all") {
		o, err := killAllOptions(parsed)
		if err != nil {
			return 2, err
		}
		return runKillAll(o)
	}
	o, err := shellOptions(command, parsed)
	if err != nil {
		return 2, err
	}
	if selected && target.Kind == "shell" {
		o.taskIdentity, o.instance = target.Identity, target.Instance
		o.hosted = target.Hosted
		if target.Connection != nil {
			applyConnectionOptions(&o, *target.Connection)
		}
	}
	return withDaemonActivity(func() (int, error) { return executeShellCommand(o) })
}

type exitStatusError interface {
	error
	ExitStatus() int
}

func runExec(args []string) (int, error) {
	parsed, err := parseCLI(args, "exec")
	if err != nil {
		return 2, err
	}
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{Host: parsed.operands[0], Command: parsed.operands[1], Args: parsed.ssh, Confirm: confirmSSHHost, Secret: promptSSHSecret}
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if exit, ok := errors.AsType[exitStatusError](err); ok {
			return exit.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected launch, spawn, attach, kill or rename; see sshore --help")
	}
	o.command = canonicalCommand(args[0])
	switch o.command {
	case "launch", "spawn", "attach", "kill", "rename":
	default:
		return o, usage("choose an action: sshore launch [HOST:], sshore spawn [HOST:], sshore attach HOST:ID, sshore kill HOST:ID, or sshore rename HOST:ID NEW")
	}
	parsed, err := parseCLI(args[1:], o.command)
	if err != nil {
		return o, err
	}
	return shellOptions(o.command, parsed)
}

func shellOptions(command string, parsed cliArguments) (options, error) {
	o := options{command: command, server: parsed.value("--server", "sshore")}
	o.ssh, o.local = parsed.ssh, parsed.local
	o.portOverrideOnly = parsed.has("--port") && len(sshio.WithoutForwarding(parsed.ssh)) == 2
	o.serverSet = parsed.has("--server")
	o.quiet, o.noEscape = parsed.has("--quiet"), parsed.has("--no-escape")
	o.detached = parsed.has("--detach")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	firstName := 1
	o.host = parsed.operand(0)
	if o.local {
		o.host, firstName = "local", 0
		if len(o.ssh) > 0 || o.serverSet {
			return o, usage("local shells do not accept SSH options")
		}
	}
	o.session = parsed.operand(firstName)
	if command == "rename" {
		o.newName = parsed.operand(firstName + 1)
	}

	if o.host != "" || o.command != "attach" {
		if err := validSSHDestination(o.host); err != nil {
			return o, err
		}
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
		return o, usage("kill requires HOST:ID, :ID or TYPE/ID")
	}
	if o.session == "" {
		return o, nil
	}
	return o, validSession(o.session)
}

// Endpoints with a value require bracketed IPv6; a host alone may omit the brackets.
func isBareSSHHost(ref string) bool {
	if !strings.Contains(ref, ":") {
		return true
	}
	address := ref
	if _, after, ok := strings.Cut(ref, "@"); ok {
		address = after
	}
	if ref != "::" && net.ParseIP(address) != nil {
		return true
	}
	if strings.HasSuffix(address, "]") {
		_, err := endpointHost(ref)
		return err == nil
	}
	return false
}

func normalizeHostOperand(command string, parsed *cliArguments) error {
	ref := parsed.operands[0]
	if isBareSSHHost(ref) {
		return validSSHDestination(ref)
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
		if command == "exec" || command == "ping" || command == "copy-id" {
			return usage("%s requires an SSH host", command)
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
	if command == "attach" && count == 0 {
		parsed.local = true
		return nil
	}
	if command == "launch" || command == "spawn" {
		host, name := parsed.operand(0), ""
		parsed.local = count == 0
		if !isBareSSHHost(host) {
			var err error
			host, name, err = splitEndpoint(host)
			if err != nil {
				return err
			}
			parsed.local = host == "here"
			if host == "origin" {
				return usage("create shells on here: or an SSH host")
			}
		}
		if name != "" {
			if err := validAlias(name); err != nil {
				return err
			}
		}
		parsed.operands = nil
		if !parsed.local {
			parsed.operands = append(parsed.operands, host)
		}
		if name != "" {
			parsed.operands = append(parsed.operands, name)
		}
		return nil
	}
	if parsed.has("--all") || !((command == "attach" || command == "kill") && count == 1 || command == "rename" && count == 2) {
		return nil
	}
	ref := parsed.operands[0]
	if !strings.Contains(ref, "/") && isBareSSHHost(ref) {
		if isTaskIDPrefix(ref) {
			parsed.bareAttachID = command == "attach"
			parsed.local = true
			return nil
		}
		if command != "attach" {
			return usage("%s requires HOST:ID, :ID or TYPE/ID", command)
		}
		return validSSHDestination(ref)
	}
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
	handoffAttachment     string
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
	quiet, noEscape       bool
	ssh                   []string
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

// A colon before the first path separator qualifies a file endpoint.
func splitFileEndpoint(value string) (string, string, error) {
	if value == "" {
		return "", "", usage("empty file operand; use here: to select a local path")
	}
	colon := strings.IndexByte(value, ':')
	slash := strings.IndexAny(value, "/\\")
	drive := colon == 1 && strings.HasPrefix(value[2:], "\\") && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z'))
	if colon < 0 || slash >= 0 && slash < colon || drive {
		return "here", value, nil
	}
	return splitEndpoint(value)
}

func remoteCopyRequest(args []string) (string, []string, string, error) {
	fail := func() (string, []string, string, error) {
		return "", nil, "", usage("use cp here:SOURCE... origin:[PATH] or cp origin:[PATH] here:DIRECTORY")
	}
	parsed, err := parseCLI(args, "cp")
	if err != nil {
		return "", nil, "", err
	}
	if len(parsed.ssh) != 0 {
		return "", nil, "", usage("remote copy requests inherit the attached connection")
	}
	args = parsed.operands
	first, firstPath, err := splitFileEndpoint(args[0])
	if err != nil {
		return "", nil, "", err
	}
	last, lastPath, err := splitFileEndpoint(args[len(args)-1])
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
		host, path, err := splitFileEndpoint(source)
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
	sourceHost, source, err := splitFileEndpoint(parsed.operands[0])
	if err != nil {
		return operation{}, err
	}
	targetHost, hint, err := splitFileEndpoint(parsed.operands[1])
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
	host, path, err := splitFileEndpoint(value)
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
	host, remote, err := splitFileEndpoint(parsed.operands[0])
	if err != nil {
		return ftpOptions{}, err
	}
	targetHost, mountpoint, err := splitFileEndpoint(parsed.operands[1])
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

type selectedTask struct {
	Kind, Name, Identity, Instance string
	Hosted                         bool
	Connection                     *recentConnection
}

// A selector with a host is resolved by that host's daemon. Local selection
// reads registered tasks together so a prefix is checked against one task set.
func resolveShortTask(command string, parsed *cliArguments) (selectedTask, bool, error) {
	var empty selectedTask
	if len(parsed.operands) == 0 || parsed.has("--all") {
		return empty, false, nil
	}
	switch command {
	case "attach", "kill", "rename":
	default:
		return empty, false, nil
	}
	kind, ref, typed := strings.Cut(parsed.operands[0], "/")
	if !typed {
		if !parsed.local {
			return empty, false, nil
		}
		ref, kind = kind, ""
	}
	var target selectedTask
	if parsed.bareAttachID {
		reply, err := metadataRPC(metadataRequest{Action: "attach_by_id", Instance: ref})
		if err != nil {
			return empty, false, err
		}
		target = *reply.Target
	} else {
		snapshot, _, err := readTaskInventory(taskInventoryRequest{Native: true, Hosted: true, Resources: command != "attach", Detail: "health"})
		if err != nil {
			return empty, false, err
		}
		target, err = selectTask(snapshot, kind, ref, nil)
		if err != nil {
			return empty, false, err
		}
	}
	if target.Connection != nil {
		parsed.local = false
		parsed.operands = []string{target.Connection.Host, target.Instance}
		return target, true, nil
	}
	if len(parsed.ssh) != 0 || parsed.has("--server") {
		return empty, false, usage("local tasks do not accept SSH options")
	}
	if target.Kind == "shell" {
		parsed.local = true
		parsed.operands[0] = target.Name
	}
	return target, true, nil
}

func selectTask(snapshot taskInventorySnapshot, kind, ref string, routes []recentConnection) (selectedTask, error) {
	var idMatch, nameMatch selectedTask
	var ambiguousID, ambiguousName, ambiguousRoute bool
	prefix := strings.ToLower(ref)
	byID := isTaskIDPrefix(prefix)
	consider := func(task selectedTask) {
		if kind != "" && kind != task.Kind && !((kind == "spawn" || kind == "launch") && task.Kind == "shell") {
			return
		}
		switch {
		case byID && strings.HasPrefix(taskUID(task.Identity), prefix):
			ambiguousID = ambiguousID || idMatch.Kind != "" && idMatch.Identity != task.Identity
			if idMatch.Identity == task.Identity && task.Connection != nil {
				// Prefer the registered local shell when SSH reaches the same task.
				if saved := idMatch.Connection; saved != nil {
					route := task.Connection
					ambiguousRoute = ambiguousRoute || saved.Host != route.Host || saved.Server != route.Server ||
						saved.Directory != route.Directory || !slices.Equal(saved.SSH, route.SSH)
				}
				return
			}
			idMatch = task
		case ref == task.Name:
			ambiguousName = ambiguousName || nameMatch.Kind != "" && nameMatch.Identity != task.Identity
			nameMatch = task
		}
	}
	for identity, row := range snapshot.Native {
		consider(selectedTask{Kind: "shell", Name: row.Name, Identity: identity})
	}
	for _, row := range snapshot.Shells {
		consider(selectedTask{Kind: "shell", Name: row.Name, Identity: identityFromUID(row.Instance), Instance: row.Instance, Hosted: true})
	}
	for _, row := range snapshot.Resources {
		consider(selectedTask{Kind: row.Kind, Name: row.Name, Identity: resourceIdentity(row)})
	}

	for i := range routes {
		route := &routes[i]
		if validTaskUID(route.Instance) && !route.viaShell() {
			consider(selectedTask{Kind: "shell", Identity: "task:" + route.Instance, Instance: route.Instance, Connection: route})
		}
	}
	switch {
	case ambiguousID:
		return selectedTask{}, usage("ambiguous task ID %s; use a longer prefix or HOST:ID", ref)
	case ambiguousRoute:
		return selectedTask{}, usage("multiple saved connections for task %s; use sshore attach HOST:ID", idMatch.Instance)
	case idMatch.Kind != "":
		return idMatch, nil
	case ambiguousName:
		return selectedTask{}, usage("ambiguous alias %s; use a task ID", ref)
	case nameMatch.Kind != "":
		return nameMatch, nil
	}
	return selectedTask{}, usage("unknown task %s; use sshore list or specify HOST:ID", ref)
}

func runMount(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteMountRequest(args)
		if err != nil {
			return 2, err
		}
		op.Path, err = absoluteLocalPath(op.Path)
		if err != nil {
			return 1, err
		}
		info, err := os.Stat(op.Path)
		if err != nil {
			return 1, err
		}
		if !info.IsDir() {
			return 1, fmt.Errorf("mount source must be a directory")
		}
		return 0, requestOperations("mount", []operation{op})
	}

	o, err := parseFTPOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"mount"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	return executeMount(o)
}

func runCopy(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		kind, paths, hint, err := remoteCopyRequest(args)
		if err != nil {
			return 2, err
		}
		ops, err := prepareFileRequest(kind, paths, hint)
		if err != nil {
			return 1, err
		}
		return 0, requestOperations("cp", ops)
	}
	o, err := parseCopyCommand(args)
	if err != nil {
		return 2, err
	}
	return executeCopyCommand(o)
}

func runEndpointForward(args []string) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteForwardRequest(args)
		if err != nil {
			return 2, err
		}
		return 0, requestOperations("forward", []operation{op})
	}
	f, foreground, err := parseForwardCommand(args)
	if err != nil {
		return 2, err
	}
	return executeEndpointForward(f, foreground)
}

func executeEndpointForward(f forwardConfig, foreground bool) (int, error) {
	stopActivity, err := beginCommandActivity()
	if err != nil {
		return 1, err
	}
	defer stopActivity()
	if f.Port == 0 || f.Target == 0 {
		input, err := newLocalInput()
		if err != nil {
			return 2, err
		}
		defer input.Close()
		label := "Local listening port"
		remote := f.Target
		if f.Reverse {
			label = "Local service port"
			remote = f.Port
		}
		termui.Log(os.Stdout, termui.Info, "Remote %s. Submit local port to forward.\n", cleanText(sshio.FormatEndpoint(f.Host, strconv.Itoa(remote))))
		port, err := readForwardPort(input, label)
		if err != nil {
			return 1, err
		}
		if f.Reverse {
			f.Target = port
		} else {
			f.Port = port
		}
		if err = input.Close(); err != nil {
			return 1, err
		}
	}
	if foreground {
		return runForwardForeground(f)
	}
	return startForwardSession(f)
}

func newLocalInput() (*termui.Input, error) {
	input, err := termui.NewLocalInput()
	if errors.Is(err, termui.ErrTerminalRequired) {
		return nil, usage("%s", err)
	}
	return input, err
}

func cleanText(s string) string { return termui.CleanText(s) }

// Display only the trailing two components of deep directories. Home is shown
// as ~, and a rune limit bounds terminal output without splitting UTF-8.
func shortDirectory(path, home string) string {
	if path == "" {
		return "—"
	}
	path = cleanText(path)
	home = strings.TrimSuffix(cleanText(home), "/")
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 3 || (len(parts) > 2 && parts[0] != "~") {
		path = "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	r := []rune(path)
	if len(r) > 40 {
		path = "…" + string(r[len(r)-39:])
	}
	return path
}

// Session and request selection share the existing line reader, with no second
// input loop or screen ownership. Filters only change visibility; original
// numbers remain stable. Bare input always denotes an exact name;
// #N unambiguously selects an index. Filtering never attaches automatically.
// Setup validates remote names and closes the reader before sending a selection.

func chooseSetupSession(input setupInput, names []string) (string, error) {
	i, err := chooseLocalItem(input, names, names, "Shell (#number/name, /filter; Enter cancels): ", "shell")
	if err != nil {
		return "", err
	}
	return names[i], nil
}

// Return the original row so callers can use the identity attached to it.
func chooseLocalItem(input setupInput, names, labels []string, prompt, kind string) (int, error) {
	return termui.ChooseItem(input, names, labels, prompt, kind)
}

// Path fields reuse readline editing/completion and retry only local validation.
// Fields start with an editable suggestion and require local submission.
// Transfer code independently checks paths at execution time.

type promptReader interface {
	Read(string, bool) (string, error)
}

type prefilledPromptReader interface {
	ReadPrefilled(string, bool, string) (string, error)
}

type reviewPromptReader interface {
	prefilledPromptReader
	ReadPath(string, termui.PathOptions) (string, error)
}

func readReviewField(input prefilledPromptReader, label string, paths bool, initial string) (string, error) {
	// Terminal control bytes must never enter the readline buffer.
	if cleanText(initial) != initial {
		return "", fmt.Errorf("invalid suggested value")
	}
	line, err := input.ReadPrefilled(label, paths, initial)
	if err != nil {
		return "", fmt.Errorf("approval cancelled: %w", err)
	}
	return line, nil
}

func readApprovalPort(input prefilledPromptReader, label, initial string) (int, error) {
	for {
		line, err := readReviewField(input, label+": ", false, initial)
		if err != nil {
			return 0, err
		}
		port, err := parsePort(strings.TrimSpace(line))
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", cleanText(err.Error()))
	}
}

func readApprovalPath(input reviewPromptReader, prompt, defaultPath string, validate func(string) (string, error)) (string, error) {
	return readReviewPath(input, prompt, defaultPath, false, func(path string) (string, error) {
		path, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return validate(path)
	})
}

func readReviewPath(input reviewPromptReader, prompt, initial string, allowEmpty bool, validate func(string) (string, error)) (string, error) {
	path, err := input.ReadPath(prompt, termui.PathOptions{Initial: initial, AllowEmpty: allowEmpty, Validate: validate})
	if err != nil {
		return "", fmt.Errorf("approval cancelled: %w", err)
	}
	return path, nil
}

func readCopyPath(input reviewPromptReader, prompt string, validate func(string) (string, error)) (string, error) {
	path, err := input.ReadPath(prompt, termui.PathOptions{Validate: func(path string) (string, error) {
		path, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return validate(path)
	}})
	if err != nil {
		return "", fmt.Errorf("copy cancelled: %w", err)
	}
	return path, nil
}

func displayBytes(size int64) string {
	for _, unit := range []struct {
		size  int64
		label string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if size >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(size)/float64(unit.size), unit.label)
		}
	}
	return fmt.Sprintf("%d B", size)
}

func readForwardPort(input promptReader, label string) (int, error) {
	for {
		value, err := input.Read(label+" (Enter starts; Ctrl-C cancels): ", false)
		if err != nil {
			return 0, fmt.Errorf("forward cancelled: %w", err)
		}
		port, err := parsePort(value)
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, termui.Warning, "%s\n", err)
	}
}
