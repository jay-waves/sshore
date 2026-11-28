package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jay-waves/sshore/internal/ports"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
)

func run(args []string) (int, error) {
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}
	if len(args) > 1 && (args[0] == "-v" || args[0] == "--verbose") {
		args = append([]string{args[1], args[0]}, args[2:]...)
	}
	command, args := canonicalCommand(args[0]), args[1:]
	switch command {
	case "--tldr":
		if len(args) != 0 {
			return 2, usage("usage: sshore --tldr or sshore COMMAND --tldr")
		}
		return 0, showTLDR("", os.Stdout)
	case "help":
		return 0, showHelp(args, os.Stdout)
	case "-h", "--help":
		return 0, showHelp(nil, os.Stdout)
	case "-V", "--version":
		if len(args) != 0 {
			return 2, usage("--version takes no arguments")
		}
		fmt.Println("sshore", version)
		return 0, nil
	case remoteServiceCommand:
		return 0, runRemoteService(args)
	}

	spec := cliCommands[command]
	if len(args) == 1 && args[0] == "--tldr" && spec.help != "" {
		return 0, showTLDR(command, os.Stdout)
	}
	parsed, err := parseCLI(args, command)
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{command}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	if spec.run == nil {
		return 2, usage("unknown command: %s", command)
	}
	return spec.run(parsed)
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

var errHelp = errors.New("help requested")

func runDaemonCommand(parsed cliArguments) (int, error) {
	switch parsed.operand(0) {
	case "":
		return 0, runDaemon()
	case "mount":
		return runMountDaemon()
	default:
		return 2, usage("usage: sshore daemon [mount]")
	}
}

func detachMessage(parsed cliArguments) (message, error) {
	m := message{Type: "detach"}
	if v := parsed.operand(0); v != "" {
		ttl, parseErr := time.ParseDuration(v)
		if parseErr != nil || ttl <= 0 {
			return m, usage("invalid duration %q; use a positive duration such as 30s, 60m or 2h", v)
		}
		m.TTLNanos = ttl.Nanoseconds()
	}
	return m, nil
}

func runDetach(parsed cliArguments) (int, error) {
	m, err := detachMessage(parsed)
	if err != nil {
		return 2, err
	}
	if identity := os.Getenv("SSHORE_LOCAL_SESSION"); identity != "" {
		return 0, retentionLocalShell(identity, m)
	}
	return 0, retentionHostedShell(m)
}

func runShell(parsed cliArguments) (int, error) {
	if parsed.remoteCommand != "" {
		return runSSHCommand(parsed)
	}
	o, err := shellOptions(parsed, selectedTask{})
	if err != nil {
		return 2, err
	}
	if parsed.command == "new" {
		host := cleanText(o.host)
		if o.local {
			host = taskManagerLabel()
		}
		launch := termui.Launch{
			Host: host, Directory: o.startupDirectory, Name: o.session,
			Args: o.argv, Pinned: o.pin, DefaultShellOnly: o.noLogin || o.noProfile,
		}
		confirmed, err := launch.Review()
		if errors.Is(err, termui.ErrTerminalRequired) {
			return 1, usage("new requires an interactive terminal")
		}
		if err != nil {
			return 1, err
		}
		if !confirmed {
			return 0, nil
		}
		o.argv = launch.Args
	}
	return executeShellCommand(o)
}

func runTargetCommand(parsed cliArguments) (int, error) {
	if parsed.command == "kill" && parsed.has("--all") {
		return runKillAll(options{host: parsed.task.host, ssh: parsed.ssh}, parsed.has("--reset"))
	}
	target, err := selectCLITask(parsed)
	if err != nil {
		return 2, err
	}
	if parsed.command == "kill" && target.Connection == nil && target.RemoteHost == "" {
		if len(parsed.ssh) != 0 {
			return 2, usage("local tasks do not accept SSH options")
		}
		return killRegisteredTask(target)
	}
	o, err := shellOptions(parsed, target)
	if err != nil {
		return 2, err
	}
	if parsed.command == "kill" {
		return runRemoteTaskStop(o)
	}
	return executeShellCommand(o)
}

func runRemoteTaskStop(o options) (int, error) {
	if !validTaskUID(o.instance) {
		return 2, usage("remote task selection requires a full task ID")
	}
	connection := remoteService(sshConnectionOptions(o), remoteServiceRequest{
		Operation: "task-control", Mode: "kill", Name: o.instance, Instance: o.instance,
	})
	if code, err := runSSHProcess(connection); code != 0 || err != nil {
		return code, err
	}
	if err := rememberShellExit(o); err != nil {
		termui.Log(os.Stderr, "", termui.Warning, "[sshore] Could not record shell exit: %s\n", cleanText(err.Error()))
	}
	return 0, nil
}

type exitStatusError interface {
	error
	ExitStatus() int
}

func runSSHCommand(parsed cliArguments) (int, error) {
	connection := sshio.Options{Host: parsed.operands[0], Command: parsed.remoteCommand, Args: parsed.ssh, Confirm: confirmSSHHost, Secret: promptSSHSecret}
	return runSSHProcess(connection)
}

func runSSHProcess(connection sshio.Options) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection = withSSHDiagnostics(connection, os.Stderr, nil)
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if exit, ok := errors.AsType[exitStatusError](err); ok {
			return exit.ExitStatus(), nil
		}
		return 1, err
	}
	return 0, nil
}

func shellOptions(parsed cliArguments, target selectedTask) (options, error) {
	command := parsed.command
	o := options{command: command}
	creating := command == "shell" || command == "new"
	if creating {
		o.command = "sh" // Shell requests use the existing protocol operation.
	}
	o.ssh, o.local = parsed.ssh, parsed.task.scope != taskSSH
	o.portOverrideOnly = parsed.has("--port") && len(sshio.RouteArgs(parsed.ssh)) == 2
	o.noEscape = parsed.has("--no-escape")
	o.argv = parsed.argv
	o.pin = parsed.has("--pin")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	o.host, o.session = parsed.task.host, parsed.task.ref
	if creating {
		o.startupDirectory, o.session = parsed.startupDirectory, parsed.value("--name", "")
	}
	if o.local {
		o.host = "local"
	}

	if target.Identity != "" {
		o.taskIdentity, o.instance, o.hosted = target.Identity, target.Instance, target.Hosted
		switch {
		case target.Connection != nil:
			o.host, o.session, o.local = target.Connection.Host, target.Instance, false
			applyConnectionOptions(&o, *target.Connection, nil)
		case target.RemoteHost != "":
			o.host, o.session, o.local = target.RemoteHost, target.Instance, false
		default:
			o.host, o.session, o.local = "local", target.Name, true
		}
	}
	if o.local && (len(o.ssh) > 0) {
		return o, usage("local shells do not accept SSH options")
	}

	if o.host != "" || o.command != "attach" {
		if err := validSSHDestination(o.host); err != nil {
			return o, err
		}
	}
	if o.session == "" {
		return o, nil
	}
	return o, validSession(o.session)
}

type options struct {
	hostInfo         hostInfo
	hold             *clientTransport
	fromTree         string
	recovering       bool
	reusable         *clientTransport
	routeTarget      bool
	sameHostRoute    bool
	handoffDone      func(context.Context, error) error
	routeParent      *options
	routeToken       string
	environment      []string
	hosted           bool
	host, session    string
	command          string
	noEscape         bool
	ssh              []string
	argv             []string
	local            bool
	pin              bool
	noLogin          bool
	noProfile        bool
	taskIdentity     string
	instance         string
	parent           string
	portOverrideOnly bool
	sshDirectory     string
	startupDirectory string
}

func executeShellCommand(o options) (int, error) {
	if o.local {
		// A local command console can outlive its last attached session.
		stop, err := leaseActivity(leaseRequest{Kind: "heartbeat"})
		if err != nil {
			return 1, err
		}
		defer stop()
	}
	if o.command == "attach" && o.taskIdentity == "" {
		var err error
		if o.local && o.session == "" {
			err = resolveDetachedAttach(&o, shellIdentityFromEnvironment())
		} else if !o.local {
			err = applyRecentConnection(&o)
		}
		if err != nil {
			return 1, err
		}
	}
	if !o.local {
		return runClient(o)
	}
	if handled, err := handoffRemoteShell(o); handled {
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	if handled, err := handoffLocalClient(o); handled {
		if err != nil {
			return 1, err
		}
		return 0, nil
	}

	return runLocalCommand(o)
}

const localEndpointPrefix = "here:"

func splitEndpoint(value string) (host, suffix string, err error) {
	switch {
	case strings.HasPrefix(value, "0:"):
		return "origin", value[2:], nil
	case strings.HasPrefix(value, "::"):
		return "", "", usage("use 0: or origin: for the attached client, or bracket an IPv6 host")
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

func remoteCopyRequest(parsed cliArguments) (string, []string, string, error) {
	fail := func() (string, []string, string, error) {
		return "", nil, "", usage("use copy here:SOURCE... origin:[PATH] or copy origin:[PATH] here:DIRECTORY")
	}
	if len(parsed.ssh) != 0 {
		return "", nil, "", usage("remote copy requests inherit the attached connection")
	}
	args := parsed.operands
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
		if err != nil || host != "here" || path == "-" || (path == "" && len(args) != 2) {
			return fail()
		}
		if path == "" && kind == "upload" {
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

func remoteMountRequest(parsed cliArguments) (operation, error) {
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
	if source == "" && !parsed.has("--lf") {
		source = "."
	}
	if len(parsed.ssh) != 0 {
		return operation{}, usage("remote mount requests inherit the attached connection")
	}
	for name := range parsed.values {
		if name != "--read-only" && name != "--lf" {
			return operation{}, usage("remote mount requests accept only --read-only and --lf")
		}
	}
	return operation{Kind: "mount", Path: source, LocalHint: hint, LF: parsed.has("--lf"), ReadOnly: parsed.has("--read-only")}, nil
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
	sources   []copyEndpoint
	target    copyEndpoint
	host      string
	upload    bool
	recursive bool
	ssh       []string
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
	return copyEndpoint{host: host, path: path, pick: path == ""}, nil
}

func parseCopyCommand(parsed cliArguments) (copyOptions, error) {
	o := copyOptions{ssh: parsed.ssh, recursive: parsed.has("-r")}
	browse := parsed.has("--lf")
	var endpoints []copyEndpoint
	for _, a := range parsed.operands {
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
		if o.recursive && (source.stream || o.target.stream) {
			return o, usage("recursive copy requires filesystem paths")
		}
		if (source.pick || browse && o.target.pick || source.stream || o.target.stream) && len(o.sources) != 1 {
			return o, usage("selection and standard streams require exactly one source")
		}
		if (source.pick && o.target.stream) || (source.stream && o.target.pick) {
			return o, usage("standard streams require an explicit file path")
		}
		if o.upload {
			if source.host != "" {
				return o, usage("remote-to-remote copies are not supported")
			}
		} else {
			if source.host == "" {
				return o, usage("copy requires a remote source or destination")
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
	if !browse {
		local, remote := ".", "~"
		sourceDefault, targetDefault := remote, local
		if o.upload {
			sourceDefault, targetDefault = local, remote
		}
		if o.sources[0].pick {
			o.sources[0].path, o.sources[0].pick = sourceDefault, false
		}
		if o.target.pick {
			o.target.path, o.target.pick = targetDefault, false
		}
	}
	return o, nil
}

func parseFTPOptions(parsed cliArguments) (ftpOptions, error) {
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
	browse := parsed.has("--lf")
	if remote == "" && !browse {
		remote = "~"
	}
	o := ftpOptions{
		Host: host, Remote: remote, Mountpoint: mountpoint,
		PickRemote: browse && remote == "", PickMountpoint: browse && mountpoint == "" && parsed.operands[1] != ".:",
		SSH:      parsed.ssh,
		ReadOnly: parsed.has("--read-only"), Stats: parsed.has("--stats"), DirectIO: parsed.has("--direct-io"),
		IOTimeoutNanos: parsed.duration("--io-timeout", defaultMountIOTimeout).Nanoseconds(), StartupTimeoutNanos: parsed.duration("--startup-timeout", 0).Nanoseconds(),
	}
	if strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return o, usage("invalid mount path")
	}
	return o, nil
}

func runMount(parsed cliArguments) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteMountRequest(parsed)
		if err != nil {
			return 2, err
		}
		if op.Path == "" {
			if err := requireFileSelectionTerminal(); err != nil {
				return 2, err
			}
			err = termui.RunLocalConsole(func(console *termui.LocalConsole) error {
				var err error
				op.Path, err = console.PickLF(context.Background(), termui.LocalFileSelection("", "Choose directory to mount", true))
				return err
			})
			if err != nil {
				return 1, err
			}
		}
		op.Path, err = sys.Abs(op.Path)
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
		return 0, requestOperations([]operation{op})
	}

	o, err := parseFTPOptions(parsed)
	if err != nil {
		return 2, err
	}
	return executeMount(o)
}

func runCopy(parsed cliArguments) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		kind, paths, hint, err := remoteCopyRequest(parsed)
		if err != nil {
			return 2, err
		}
		ops, err := prepareRemoteCopy(kind, paths, hint, parsed.has("-r"), parsed.has("--lf"))
		if err != nil {
			return 1, err
		}
		return 0, requestOperations(ops)
	}
	o, err := parseCopyCommand(parsed)
	if err != nil {
		return 2, err
	}
	return executeCopyCommand(o)
}

func runEndpointForward(parsed cliArguments) (int, error) {
	attached := os.Getenv("SSHORE_SOCKET") != ""
	if attached && (len(parsed.ssh) != 0 || parsed.has("--retries")) {
		return 2, usage("remote forward requests inherit the attached connection")
	}
	host, rule, err := parseForwardEndpoints(parsed.operands, attached)
	if err != nil {
		return 2, err
	}
	rule.UDP = parsed.has("--udp")
	if attached {
		return 0, requestOperations([]operation{{Kind: "forward", Forward: rule}})
	}
	rule.LocalPort = cmp.Or(rule.LocalPort, rule.RemotePort)
	return startForwardSession(forwardConfig{Host: host, SSH: parsed.ssh, Retries: parsed.integer("--retries"), Rule: rule})
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

type prefilledPromptReader interface {
	ReadPrefilled(string, bool, string) (string, error)
}

type reviewPromptReader interface {
	prefilledPromptReader
	ReadPath(string, termui.PathOptions) (string, error)
}

func readForwardPort(input prefilledPromptReader, rule ports.Rule) (int, error) {
	initial := strconv.Itoa(cmp.Or(rule.LocalPort, rule.RemotePort))
	prompt := localForwardPortLabel(rule.Reverse) + " (Enter to approve, Ctrl-C to cancel): "
	for {
		line, err := input.ReadPrefilled(prompt, false, initial)
		if err != nil {
			return 0, fmt.Errorf("approval cancelled: %w", err)
		}
		port, err := parsePort(strings.TrimSpace(line))
		if err == nil {
			return port, nil
		}
		termui.Log(os.Stdout, "", termui.Warning, "%s\n", cleanText(err.Error()))
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
