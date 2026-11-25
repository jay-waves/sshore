package main

import (
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

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
)

func run(args []string) (int, error) {
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
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
	if !spec.recovery && !activityAllowed() {
		return 1, disabledFeaturesError()
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

func killAllOptions(parsed cliArguments) (options, error) {
	o := options{host: parsed.operand(0), ssh: parsed.ssh, server: parsed.value("--server", "sshore")}
	if o.host == "" && (len(o.ssh) != 0 || o.server != "sshore") {
		return o, validSSHDestination(o.host)
	}
	return o, nil
}

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

func runTargetCommand(parsed cliArguments) (int, error) {
	command := parsed.command
	if command == "kill" && parsed.has("--all") {
		o, err := killAllOptions(parsed)
		if err != nil {
			return 2, err
		}
		return runKillAll(o)
	}
	target, err := selectCLITask(parsed)
	if err != nil {
		return 2, err
	}
	if target.Identity != "" && target.Connection == nil && target.RemoteHost == "" && command != "attach" {
		if len(parsed.ssh) != 0 || parsed.has("--server") {
			return 2, usage("local tasks do not accept SSH options")
		}
		return mutateRegisteredTask(command, target, parsed.operand(1))
	}
	o, err := shellOptions(parsed, target)
	if err != nil {
		return 2, err
	}
	return withDaemonActivity(func() (int, error) { return executeShellCommand(o) })
}

type exitStatusError interface {
	error
	ExitStatus() int
}

func runExec(parsed cliArguments) (int, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection := sshio.Options{Host: parsed.operands[0], Command: parsed.operands[1], Args: parsed.ssh, Confirm: confirmSSHHost, Secret: promptSSHSecret}
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
	o := options{command: command, server: parsed.value("--server", "sshore")}
	if command == "shell" {
		o.command = "sh" // Shell requests use the existing protocol operation.
	}
	o.ssh, o.local = parsed.ssh, parsed.task.scope != taskSSH
	o.portOverrideOnly = parsed.has("--port") && len(sshio.WithoutForwarding(parsed.ssh)) == 2
	o.serverSet = parsed.has("--server")
	o.noEscape = parsed.has("--no-escape")
	o.pin = parsed.has("--pin")
	o.noLogin = parsed.has("--no-login")
	o.noProfile = parsed.has("--no-profile")
	o.host, o.session = parsed.task.host, parsed.task.ref
	if o.local {
		o.host = "local"
	}
	if command == "rename" {
		o.newName = parsed.operand(1)
	}
	if target.Identity != "" {
		o.taskIdentity, o.instance, o.hosted = target.Identity, target.Instance, target.Hosted
		switch {
		case target.Connection != nil:
			o.host, o.session, o.local = target.Connection.Host, target.Instance, false
			applyConnectionOptions(&o, *target.Connection)
		case target.RemoteHost != "":
			o.host, o.session, o.local = target.RemoteHost, target.Instance, false
		default:
			o.session = target.Name
		}
	}
	if o.local && (len(o.ssh) > 0 || o.serverSet) {
		return o, usage("local shells do not accept SSH options")
	}
	o.openIfUnavailable = command == "attach" && !o.local && isTaskIDPrefix(o.session)

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
		parsed.operands = nil
	default:
		parsed.operands[0] = host
	}
	return nil
}

func parseTaskSelector(command, ref string) (taskSelector, error) {
	if command == "shell" {
		if ref == "" {
			return taskSelector{}, nil
		}
		if isBareSSHHost(ref) {
			return taskSelector{scope: taskSSH, host: ref}, validSSHDestination(ref)
		}
		host, name, err := splitEndpoint(ref)
		if err != nil {
			return taskSelector{}, err
		}
		if host == "origin" {
			return taskSelector{}, usage("create shells on here: or an SSH host")
		}
		if name != "" {
			if err := validAlias(name); err != nil {
				return taskSelector{}, err
			}
		}
		return scopedTask(host, name), nil
	}

	if uid, ok := strings.CutPrefix(ref, "task:"); ok && validTaskUID(uid) {
		ref = uid
	}
	if kind, selector, typed := strings.Cut(ref, "/"); typed {
		switch kind {
		case "sh", "shell":
			kind = "shell"
		case "mount", "forward":
			if command == "attach" {
				return taskSelector{}, usage("attach requires a shell")
			}
		default:
			return taskSelector{}, usage("unknown task type %q", kind)
		}
		scope := taskLocal
		if kind == "shell" && isTaskIDPrefix(selector) && command == "attach" {
			scope = taskKnown
		}
		return taskSelector{scope: scope, kind: kind, ref: selector}, validSession(selector)
	}
	if ref == "" && command == "attach" {
		return taskSelector{}, nil
	}
	if isTaskIDPrefix(ref) {
		return taskSelector{scope: taskKnown, ref: ref}, nil
	}
	if isBareSSHHost(ref) {
		if command == "attach" {
			return taskSelector{scope: taskSSH, host: ref}, validSSHDestination(ref)
		}
		return taskSelector{}, usage("%s requires a task ID, HOST:ID, :ID or TYPE/ID", command)
	}

	host, selector, err := splitEndpoint(ref)
	if err != nil {
		return taskSelector{}, err
	}
	if selector == "" && command != "attach" {
		return taskSelector{}, usage("%s requires a task ID or alias after the colon", command)
	}
	if host == "origin" {
		return taskSelector{}, usage("shell selectors require here: or an SSH host")
	}
	if selector != "" {
		if uid, ok := strings.CutPrefix(selector, "task:"); ok && validTaskUID(uid) {
			selector = uid
		}
		if err := validSession(selector); err != nil {
			return taskSelector{}, err
		}
	}
	return scopedTask(host, selector), nil
}

func scopedTask(host, ref string) taskSelector {
	if host == "here" {
		return taskSelector{scope: taskLocal, ref: ref}
	}
	return taskSelector{scope: taskSSH, host: host, ref: ref}
}

type options struct {
	hold                  *clientTransport
	fromTree              string
	recovering            bool
	openIfUnavailable     bool
	reusable              *clientTransport
	routeTarget           bool
	sameHostRoute         bool
	handoffDone           func(context.Context, error) error
	routeParent           *options
	routeToken            string
	environment           []string
	hosted                bool
	host, session, server string
	command               string
	newName               string
	noEscape              bool
	ssh                   []string
	local                 bool
	pin                   bool
	noLogin               bool
	noProfile             bool
	taskIdentity          string
	instance              string
	parent                string
	remoteOwned           bool
	remoteParentKey       string
	portOverrideOnly      bool
	serverSet             bool
	sshDirectory          string
}

func executeShellCommand(o options) (int, error) {
	managedRemoteAttach := o.command == "attach" && !o.local && (os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSHORE_LOCAL_SESSION") != "")
	if managedRemoteAttach && o.session == "" && o.instance == "" {
		return 1, usage("select a child with sshore attach HOST:ID")
	}
	if o.command == "attach" && o.taskIdentity == "" {
		var err error
		if o.local && o.session == "" {
			err = resolveDetachedAttach(&o, shellIdentityFromEnvironment())
		} else if !o.local && (!managedRemoteAttach || o.openIfUnavailable) {
			err = applyRecentConnection(&o)
		}
		if err != nil {
			return 1, err
		}
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
	if managedRemoteAttach {
		return 1, fmt.Errorf("remote attach from a managed shell requires an active terminal")
	}
	if o.local {
		return runLocalCommand(o)
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

func remoteCopyRequest(parsed cliArguments) (string, []string, string, error) {
	fail := func() (string, []string, string, error) {
		return "", nil, "", usage("use cp here:SOURCE... origin:[PATH] or cp origin:[PATH] here:DIRECTORY")
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

func parseCopyCommand(parsed cliArguments) (copyOptions, error) {
	o := copyOptions{ssh: parsed.ssh}
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

func runMount(parsed cliArguments) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		op, err := remoteMountRequest(parsed)
		if err != nil {
			return 2, err
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
		return 0, requestOperations("mount", []operation{op})
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
		ops, err := prepareFileRequest(kind, paths, hint)
		if err != nil {
			return 1, err
		}
		return 0, requestOperations("cp", ops)
	}
	o, err := parseCopyCommand(parsed)
	if err != nil {
		return 2, err
	}
	return executeCopyCommand(o)
}

func runEndpointForward(parsed cliArguments) (int, error) {
	if os.Getenv("SSHORE_SOCKET") != "" {
		if len(parsed.ssh) != 0 || len(parsed.values) != 0 {
			return 2, usage("remote forward requests inherit the attached connection")
		}
		_, op, err := parseForwardEndpoints(parsed.operands, true)
		if err != nil {
			return 2, err
		}
		return 0, requestOperations("forward", []operation{op})
	}
	f, foreground, err := parseForwardCommand(parsed)
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
		remote := f.Target
		if f.Reverse {
			remote = f.Port
		}
		termui.Log(os.Stdout, "", termui.Info, "Remote %s. Submit local port to forward.\n", cleanText(sshio.FormatEndpoint(f.Host, strconv.Itoa(remote))))
		port, err := readForwardPort(input, localForwardPortLabel(f.Reverse))
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
	prompt := label + " (Enter to approve, Ctrl-C to cancel): "
	for {
		line, err := readReviewField(input, prompt, false, initial)
		if err != nil {
			return 0, err
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
		termui.Log(os.Stdout, "", termui.Warning, "%s\n", err)
	}
}
