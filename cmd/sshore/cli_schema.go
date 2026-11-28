package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/shell"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

// Aliases resolve only command names, never operands or SSH arguments.
var commandAliases = make(map[string]string)

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
	"--lf":        {},
	"--udp":       {},
	"-r":          {},
	"--name":      {kind: cliText},
	"--apply":     {},
	"--port":      {kind: cliPort, alias: "-p"},
	"--verbose":   {alias: "-v"},
	"-J":          {kind: cliText},
	"-i":          {kind: cliText},
	"-F":          {kind: cliText},
	"--no-escape": {}, "--pin": {}, "--no-login": {}, "--no-profile": {},
	"--all":       {},
	"--reset":     {},
	"--read-only": {}, "--stats": {}, "--direct-io": {},
	"--io-timeout": {kind: cliDuration}, "--startup-timeout": {kind: cliDuration},
	"--retries": {kind: cliRetries},
	"--count":   {kind: cliPingCount},
}

type cliSpec struct {
	flags, aliases []string
	ssh, program   bool
	min, max       int
	selector       cliSelectorKind
	group          cliGroup
	summary        string
	help, examples string
	run            func(cliArguments) (int, error)
}

type cliSelectorKind uint8

const (
	cliNoSelector cliSelectorKind = iota
	cliHostSelector
	cliStartupSelector
	cliAttachSelector
	cliKillSelector
)

func (s cliSpec) option(name string) (string, cliOption, bool) {
	if s.ssh {
		for _, canonical := range [...]string{"--port", "-J", "-i", "-F", "--verbose"} {
			option := cliOptions[canonical]
			if name == canonical || name == option.alias {
				return canonical, option, true
			}
		}
	}
	if slices.Contains(s.flags, name) {
		return name, cliOptions[name], true
	}
	return "", cliOption{}, false
}

type cliArguments struct {
	command          string
	task             taskSelector
	startupDirectory string
	operands         []string
	values           map[string]string
	ssh              []string
	argv             []string
	remoteCommand    string
}

type taskScope uint8

const (
	taskLocal taskScope = iota
	taskKnown
	taskSSH
)

type taskSelector struct {
	scope           taskScope
	host, ref, name string
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
	if name == "--name" {
		return validAlias(value)
	}
	if name == "--direct-io" && runtime.GOOS != "linux" {
		return usage("--direct-io is supported only on Linux")
	}
	return nil
}

// The literal suffix ends both sshore's options and the optional SSH block.
// Parse each block independently without starting work or reading state.
func parseCLI(args []string, command string) (cliArguments, error) {
	command = canonicalCommand(command)
	a := cliArguments{command: command, values: make(map[string]string)}
	spec, ok := cliCommands[command]
	if !ok {
		return a, usage("unknown command: %s", command)
	}
	var literalArgs, sshArgs, identityArgs []string
	var hasSSH bool
	end := slices.Index(args, "--")
	if end >= 0 {
		literalArgs, args = args[end+1:], args[:end]
	}
	if spec.ssh {
		if end := slices.Index(args, "--ssh"); end >= 0 {
			hasSSH, sshArgs, args = true, args[end+1:], args[:end]
		}
	}
	if spec.help != "" {
		for _, arg := range args {
			if arg == "--help" || arg == "-h" {
				return a, errHelp
			}
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" {
			return a, usage("empty argument")
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
		name, value, attached := strings.Cut(arg, "=")
		canonical, option, found := spec.option(name)
		if !found && spec.ssh {
			short, suffix, n := sshio.SplitOption([]string{arg})
			switch short {
			case "-J", "-p", "-i", "-F":
				if n == 1 {
					name, value, attached = short, suffix, true
					canonical, option, found = spec.option(name)
				}
			case "":
			default:
				return a, usage("SSH option %s must follow --ssh", arg)
			}
		}
		if !found {
			return a, usage("unsupported option: %s", name)
		}
		if a.has(canonical) {
			return a, usage("%s may only be specified once", canonical)
		}
		if option.kind != cliBool {
			if !attached {
				i++
				if i == len(args) || strings.HasPrefix(args[i], "--") ||
					(name == "-J" || name == "-p" || name == "-i" || name == "-F") && strings.HasPrefix(args[i], "-") {
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
		if canonical == "-i" {
			identityArgs = append(identityArgs, "-i", value)
			continue
		}
		a.values[canonical] = value
		if err := validateCLIValue(canonical, value, option); err != nil {
			return a, err
		}
	}
	if spec.program {
		if end >= 0 && (len(literalArgs) == 0 || literalArgs[0] == "") {
			return a, usage("expected -- followed by an executable")
		}
		if end >= 0 && (a.has("--no-login") || a.has("--no-profile")) {
			return a, usage("--no-login and --no-profile require starting the default shell")
		}
		a.argv = literalArgs
	} else if command == "shell" && end >= 0 {
		a.remoteCommand = strings.Join(literalArgs, " ")
		if strings.TrimSpace(a.remoteCommand) == "" {
			return a, usage("expected a command after --")
		}
		if len(a.operands) != 1 {
			return a, usage("shell command execution requires one SSH host")
		}
		for _, flag := range []string{"--name", "--pin", "--no-login", "--no-profile", "--no-escape"} {
			if a.has(flag) {
				return a, usage("%s requires an interactive shell", flag)
			}
		}
	} else {
		if slices.Contains(literalArgs, "") {
			return a, usage("empty argument")
		}
		a.operands = append(a.operands, literalArgs...)
	}
	var err error
	var sshOptionsUsed sshOptionPresence
	if hasSSH {
		if len(sshArgs) == 0 {
			return a, usage("expected SSH options after --ssh")
		}
		a.ssh, sshOptionsUsed, err = parseSSHOptions(sshArgs)
		if err != nil {
			return a, err
		}
	}
	if a.has("--port") {
		if sshOptionsUsed.port {
			return a, usage("--port cannot be combined with SSH -p or -o Port")
		}
		a.ssh = append([]string{"-p", a.values["--port"]}, a.ssh...)
	}
	if a.has("-J") {
		if sshOptionsUsed.jump {
			return a, usage("-J cannot be combined with SSH -J or -o ProxyJump after --ssh")
		}
		a.ssh = append([]string{"-J", a.values["-J"]}, a.ssh...)
	}
	if a.has("-F") {
		if sshOptionsUsed.config {
			return a, usage("-F cannot be combined with SSH -F after --ssh")
		}
		a.ssh = append([]string{"-F", a.values["-F"]}, a.ssh...)
	}
	if a.has("--verbose") {
		a.ssh = append([]string{"-v"}, a.ssh...)
	}
	a.ssh = append(identityArgs, a.ssh...)
	if len(a.operands) < spec.min {
		return a, usage("not enough arguments for %s; see sshore %s --help", command, command)
	}
	if spec.max >= 0 && len(a.operands) > spec.max {
		return a, usage("unexpected argument: %s", a.operands[spec.max])
	}
	switch spec.selector {
	case cliHostSelector:
		return a, validateHostOperand(command, a.operand(0))
	case cliStartupSelector:
		if a.remoteCommand != "" {
			return a, validateHostOperand(command, a.operand(0))
		}
		a.task, a.startupDirectory, err = parseStartupDestination(a.operand(0))
	case cliAttachSelector:
		a.task, err = parseAttachTarget(a.operand(0))
	case cliKillSelector:
		a.task, err = parseKillTarget(a)
	}
	if err == nil && a.task.scope == taskLocal && (len(a.ssh) != 0) && spec.selector != cliNoSelector {
		err = usage("local tasks do not accept SSH options")
	}
	return a, err
}

type sshOptionPresence struct{ port, jump, config bool }

func parseSSHOptions(suffix []string) ([]string, sshOptionPresence, error) {
	var ssh []string
	var used sshOptionPresence
	for len(suffix) > 0 {
		name, value, n := sshio.SplitOption(suffix)
		if name == "" {
			return nil, used, usage("unsupported SSH option after --ssh: %s; put sshore arguments before --ssh", suffix[0])
		}
		if n == 0 || value == "--" {
			return nil, used, usage("missing value for %s", name)
		}
		used.port = used.port || sshio.PortOption(name, value)
		used.jump = used.jump || name == "-J" || name == "-o" && sshio.ConfigKey(value) == "proxyjump"
		used.config = used.config || name == "-F"
		ssh = append(ssh, name)
		if value != "" {
			ssh = append(ssh, value)
		}
		suffix = suffix[n:]
	}
	return ssh, used, nil
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

func validateHostOperand(command, ref string) error {
	if !isBareSSHHost(ref) || strings.ContainsAny(ref, "/\\") {
		return usage("%s requires an SSH host", command)
	}
	return validSSHDestination(ref)
}

func parseStartupDestination(ref string) (taskSelector, string, error) {
	if ref == "" || isBareSSHHost(ref) {
		target, err := parseTaskAccount(ref)
		return target, "", err
	}
	host, path, err := splitEndpoint(ref)
	if err != nil {
		return taskSelector{}, "", err
	}
	if !shell.ValidStartupPath(path) {
		return taskSelector{}, "", usage("invalid startup path %q; use an absolute path, ./ or ../; use --name for a task name", path)
	}
	target, err := scopedTaskAccount(host)
	return target, path, err
}

func taskIDOperand(ref string) string {
	if id, ok := strings.CutPrefix(ref, "task:"); ok && validTaskUID(id) {
		return id
	}
	if isTaskIDPrefix(ref) {
		return strings.ToLower(ref)
	}
	return ""
}

func parseAttachTarget(ref string) (taskSelector, error) {
	if id := taskIDOperand(ref); id != "" {
		return taskSelector{scope: taskKnown, ref: id}, nil
	}
	target, err := parseTaskAccount(ref)
	if err != nil {
		return taskSelector{}, usage("attach expects a task ID, name or host: %s", err)
	}
	if target.scope == taskSSH && validAlias(ref) == nil {
		target.name = ref
	}
	return target, nil
}

func parseKillTarget(a cliArguments) (taskSelector, error) {
	if a.has("--all") {
		return parseTaskAccount(a.operand(0))
	}
	if a.has("--reset") {
		return taskSelector{}, usage("--reset requires --all")
	}
	ref := a.operand(0)
	if id := taskIDOperand(ref); id != "" {
		return taskSelector{scope: taskKnown, ref: id}, nil
	}
	if validAlias(ref) == nil {
		return taskSelector{scope: taskKnown, name: ref}, nil
	}
	return taskSelector{}, usage("kill requires a task ID or name; use kill HOST --all for a remote account")
}

func parseTaskAccount(ref string) (taskSelector, error) {
	if ref == "" {
		return taskSelector{}, nil
	}
	if !isBareSSHHost(ref) || strings.ContainsAny(ref, "/\\") {
		return taskSelector{}, usage("invalid host")
	}
	return taskSelector{scope: taskSSH, host: ref}, validSSHDestination(ref)
}

func scopedTaskAccount(host string) (taskSelector, error) {
	switch host {
	case "here":
		return taskSelector{}, nil
	case "origin":
		return taskSelector{}, usage("select here: or an SSH host")
	}
	return taskSelector{scope: taskSSH, host: host}, nil
}

type cliGroup uint8

const (
	groupShell cliGroup = iota
	groupSSH
	groupTask
	groupAccount
)

var cliGroupNames = [...]string{
	groupShell:   "Shells",
	groupSSH:     "SSH, files and ports",
	groupTask:    "Tasks",
	groupAccount: "Account maintenance",
}

const helpFooter = `Start: sshore shell work  Resume: sshore attach 7afe22
In a session: Ctrl+B c opens a nested shell; d pins and detaches; r redraws.
Ctrl+B a attaches the latest eligible session; s or : opens the command prompt.
Examples: sshore --tldr             Details: sshore COMMAND --help
Version: sshore --version
`

const sshOptions = `
-p PORT (alias: --port) selects the SSH port (1–65535).
-J DEST selects a jump host. -i FILE adds a private key file and may be repeated.
-F FILE selects an SSH configuration file. -F none uses built-in defaults.
-v (alias: --verbose) prints SSH connection, authentication and negotiation details to stderr.
Other SSH options follow --ssh.
Do not repeat a top-level port, jump host or configuration file after --ssh.
Configuration options: -p -i -J -F -l -o -c -m -v/-vv/-vvv -4 -6.
An interactive terminal is required for passphrases, passwords and
keyboard-interactive login.
Forwarding rules in SSH config and -L, -R, -D produce a warning and are ignored.
Use sshore forward for port forwarding. Compression, agent/X11 forwarding and
ProxyCommand are unsupported.
`

const connectionOptions = `  --no-escape           Disable Ctrl+B shortcuts
`

const startupLocationHelp = `Use HOST:PATH or here:PATH for a startup directory. PATH must be absolute
or start with ./ or ../. Quote paths that contain spaces.

  --name NAME           Set a name for the new session
`

const shellStartupOptions = `  --pin                 Keep the task running after disconnect
  --no-login            Use non-login startup (Linux and macOS)
  --no-profile          Skip optional shell startup files; implies --no-login on Linux and macOS
`

const shellConnectionHelp = `
Managed connections deploy the matching server through SSH and SFTP.
` + sshOptions

const connectionFlags = "--no-escape"
const terminalStartupFlags = connectionFlags + " --name --pin --no-login --no-profile"

var cliCommands map[string]cliSpec

func init() {
	cliCommands = map[string]cliSpec{
		"": {
			flags: []string{"--version"},
			examples: `Start a temporary remote shell
  sshore shell work
Resume it after detaching with Ctrl+B d
  sshore attach 7afe22
Upload a file
  sshore copy ./report.txt work:/tmp/report.txt
Mirror a remote service on a local port
  sshore forward work:3000 8080

More examples: sshore shell|copy|mount|forward --tldr
`,
		},
		"keygen": {
			run: runKeygen, group: groupSSH, summary: "Generate an Ed25519 SSH key pair",
			max: 1,
			help: `Usage: sshore keygen [PATH]

Generate an OpenSSH Ed25519 key pair. The default private key path is
~/.ssh/id_ed25519. The public key uses the same path with a .pub suffix.
Existing files are kept. Press Enter at the passphrase prompt for no passphrase.
`,
		},
		"copy-id": {
			run: runCopyID, selector: cliHostSelector, group: groupSSH, summary: "Install an SSH public key on a host",
			ssh: true, min: 1, max: 1,
			help: `Usage: sshore copy-id HOST [--ssh SSH_OPTIONS...]

Install a public key from the host's effective IdentityFile settings into
the remote account's ~/.ssh/authorized_keys. If several keys are available,
choose one in the terminal with Tab completion.
` + sshOptions,
			examples: "Install the configured public key\n  sshore copy-id work\n",
		},
		"install": {
			run: runInstall, group: groupAccount, summary: "Install the local sshore client",
			max: 1, flags: []string{"--apply"},
			help: `Usage: sshore install [here:[PATH]]

Install the latest client release. Review running tasks before replacement.
Installation preserves settings and history. Use kill --all --reset to clear state.
Run outside managed shells. Use here:PATH to specify an absolute executable path.
`,
			examples: "Install the latest client release\n  sshore install\n",
		},
		"ping": {
			run: runPing, selector: cliHostSelector, group: groupSSH, summary: "Measure SSH connection and echo latency",
			ssh: true, min: 1, max: 1, flags: []string{"--count"},
			help: `Usage: sshore ping HOST [--count N] [--port PORT] [--ssh SSH_OPTIONS...]

Measure authenticated SSH connection time and character round-trip latency
over one SSH session. The remote account must be able to run cat.
--count accepts 1 to 1000 total samples (default 30). Each group has up to
10 samples; groups are 1 second apart. Echo testing has a 30s budget, excluding
the intervals.
` + sshOptions,
			examples: "Measure latency to an SSH host\n  sshore ping work --count 20\n",
		},
		"shell": {
			run: runShell, selector: cliStartupSelector, group: groupShell, summary: "Start a shell or execute a command over SSH", aliases: []string{"sh"},
			ssh: true, max: 1, flags: strings.Fields(terminalStartupFlags),
			help: `Usage: sshore shell [HOST[:PATH]] [--name NAME] [options] [--ssh SSH_OPTIONS...]

       sshore shell HOST [--ssh SSH_OPTIONS...] -- COMMAND [ARG...]

Alias: sh

Start a shell on this machine or an SSH host. A shell ends with its starting
connection or parent shell unless started with --pin or explicitly detached.
Opening a shell from an interactive session creates a child. Its parent waits for it to return.

With -- COMMAND, join the words with spaces and execute once over SSH without
a remote PTY or sshore installation. Standard streams and exit status pass through.

` + startupLocationHelp + shellStartupOptions + connectionOptions + shellConnectionHelp,
			examples: "Open a remote shell\n  sshore sh work --name build\nOpen a local shell\n  sshore sh --name build\n",
		},
		"new": {
			run: runShell, selector: cliStartupSelector, group: groupShell, summary: "Start a terminal for a shell or program",
			ssh: true, program: true, max: 1, flags: strings.Fields(terminalStartupFlags),
			help: `Usage: sshore new [HOST[:PATH]] [--name NAME] [options] [--ssh SSH_OPTIONS...] [-- PROGRAM [ARG...]]

Omit -- to open an interactive shell. The task ends with its starting connection
or parent shell unless started with --pin or explicitly detached.
The arguments after -- go directly to PROGRAM. Quote paths with spaces in
your shell before invoking sshore.
Review the launch in the full-screen prompt. Press Enter to start, e to edit
the command line, or Esc to cancel.
Inside a managed session, the command returns the result of handing off the
terminal. Control returns to the parent terminal when the child ends or detaches.

` + startupLocationHelp + shellStartupOptions + connectionOptions + shellConnectionHelp,
			examples: "Open a shell\n  sshore new work\nKeep an editor running after disconnect\n  sshore new work:/srv/project --name editor --pin -- nvim .\n",
		},
		"attach": {
			run: runTargetCommand, selector: cliAttachSelector, group: groupShell, summary: "Resume a detached session", aliases: []string{"a"},
			ssh: true, max: 1, flags: strings.Fields(connectionFlags),
			help: `Usage: sshore attach [ID|NAME|HOST] [options] [--ssh SSH_OPTIONS...]

With no operand, resume the most recently detached eligible local session.
An ID selects a task. Other operands match a known session name first, then
the most recently detached eligible session on that SSH host. If a name is
ambiguous, use an ID.

Options:
` + connectionOptions + shellConnectionHelp,
		},
		"detach": {
			run: runDetach, group: groupShell, summary: "Pin and leave the current shell", aliases: []string{"d"},
			max: 1,
			help: `Usage: sshore detach [DURATION]

Pin the current shell and return to its parent or the root prompt.
Detach keeps its parent edge. Omit DURATION to keep the shell until it exits.
Use attach to resume it. Ctrl+B d also pins and detaches.
`,
		},
		"pstree": {
			run: runPstree, group: groupTask, summary: "Show the task tree", aliases: []string{"ps"},
			help: `Usage: sshore pstree  (alias: ps)

Show tasks under their host roots, with type/ID labels and terminal parent edges.
A * after the ID marks the current shell. Use list for details.

` + inventoryStateHelp,
		},
		"list": {
			run: runList, group: groupTask, summary: "Show task details", aliases: []string{"ls"},
			help: `Usage: sshore list  (alias: ls)

List terminal sessions, mounts and forwards, including read-only resources under origin.
A * after the ID marks the current shell.
PARENT shows the parent session's short ID. For a terminal, DETAIL starts with
the executable name. A forward's arrow points toward the target service.

` + inventoryStateHelp,
		},
		"kill": {
			run: runTargetCommand, selector: cliKillSelector, group: groupTask, summary: "End a task or shut down the account daemon",
			ssh: true, max: 1, flags: []string{"--all", "--reset"},
			help: `Usage: sshore kill ID|NAME [--ssh SSH_OPTIONS...]
       sshore kill [HOST] --all [--reset] [--ssh SSH_OPTIONS...]

Select a known shell, mount or forward by its ID or name. An ambiguous name
requires an ID. Stop resources listed under origin on the account that owns them.

Effects:
  Shell     Kills the shell and its identified processes without confirmation.
            Unsaved work may be lost.
  Mount     Unmounts first; a busy mount stays active for retry.
  Forward   Closes the forward and its active connections.

--all stops the local daemon when HOST is omitted. With HOST it stops this
client's matching remote daemon and local resources connected to HOST.
It shows running tasks before shutdown. Answer y to end them.

--reset also clears local sshore state and caches when HOST is omitted. With
HOST it removes this client's current remote server and its state. Reset always
requires confirmation. Run local reset outside managed shells.
` + sshOptions,
		},
		"copy": {
			run: runCopy, group: groupSSH, summary: "Copy files over SSH", aliases: []string{"cp"},
			ssh: true, min: 2, max: -1, flags: []string{"-r", "--lf"},
			help: `Usage: sshore copy [-r] [--lf] SOURCE... TARGET [--ssh SSH_OPTIONS...]

Alias: cp

A bare path belongs to the machine running this command. Use HOST:PATH
for an SSH host. Use here: or .: for an explicit local endpoint.
In an attached remote shell, origin: or 0: requests local approval.
Use : (also .: or here:) and --lf to browse on the host before sending to
origin. Use origin: as an empty source with --lf to browse during approval.
An empty local path uses the current directory and an empty SSH path uses the
remote home. An empty source selects a directory and requires -r unless you
use --lf to browse. Use - for standard input or output
with an explicit file.
Copy accepts up to 16 source arguments. Multiple sources require a destination directory.
Use -r to copy directories over SSH, including empty directories. Recursive
copy rejects symbolic links and does not overwrite existing files.
Recursive copy requires filesystem paths. With origin:, approval is per directory.

  sshore copy report.zip work:/tmp/
  sshore cp -r ./project work:/tmp/
  sshore copy here: work:
  sshore copy work:/tmp/report.txt -
  sshore copy - work:/tmp/report.txt

A failure stops the batch and keeps completed files. Stdout may already
contain partial data. The remote sshore release must match exactly.
` + sshOptions,
			examples: `Upload a file
  sshore copy ./report.txt work:/tmp/report.txt
Download a file
  sshore copy work:/tmp/report.txt ./report.txt
Use a different SSH port
  sshore copy ./report.txt work:/tmp/report.txt --port 2222
`,
		},
		"mount": {
			run: runMount, group: groupSSH, summary: "Mount a remote directory", aliases: []string{"mnt"},
			ssh: true, min: 2, max: 2, flags: strings.Fields("--read-only --io-timeout --startup-timeout --stats --direct-io --lf"),
			help: `Usage: sshore mount HOST:SOURCE MOUNTPOINT [options] [--ssh SSH_OPTIONS...]
       sshore mount here:[DIRECTORY] origin:[MOUNTPOINT] [--read-only]

Mount a remote directory over SFTP. The client deploys and starts a remote
helper with the same release version.
On a Windows host, use a drive-letter path such as HOST:E:/project.
Mounts run in the background. Use sshore kill TASK to unmount.
HOST: and HOST:~ mount the remote home.
Use [HOST]:SOURCE for an IPv6 address.
Use : for an automatic mountpoint or add --lf to browse omitted paths.
Pass .: to allocate a mountpoint even with --lf. Linux and macOS
use ~/mnt/NAME. Windows selects a free drive letter.
Requires FUSE on Linux, WinFsp on Windows, or macFUSE with its kernel extension
on Apple Silicon Macs (experimental).
A custom mountpoint must be an existing empty directory on Linux or macOS, or a
free drive letter or nonexistent directory on Windows.

Options:
  --read-only                  Reject changes through the mount
  --io-timeout DURATION        Per SFTP request, including handshake (60s; 0s disables)
  --startup-timeout DURATION   Startup including authentication (0s disables)
  --stats                      Report request and cache statistics
  --direct-io                  Disable kernel read caching (Linux)

From a remote shell, origin: and 0: select the attached client.
Use : (also .: or here:) as the source with --lf to browse on the host.
Approve the request on the attached client and choose a mountpoint.
If the filesystem reports a busy mount, close files and retry sshore kill TASK.
Request timeouts close the whole mount connection;
affected writes are not retried.

Example: sshore mount work:/srv/project .: --read-only
` + sshOptions,
			examples: `Mount a remote directory at the default local mountpoint
  sshore mount work:/srv/project .:
Mount read-only
  sshore mount work:/srv/project .: --read-only

Requires FUSE on Linux, WinFsp on Windows, or macFUSE with its kernel extension
on Apple Silicon Macs (experimental).
`,
		},
		"lf": {
			run: runFileBrowser, group: groupSSH, ssh: true, max: 2,
			summary:  "Browse files and open a shell, copy or mount",
			help:     "Usage: sshore lf [HOST | SOURCE PEER] [options]\n\nBrowse the current directory or the given host's home. Press s to open a shell.\nPress o to open a local file or directory with the system default app.\nUse l to enter a directory or open a local file, e for EDITOR and i for PAGER.\nPress y to copy the selected path.\nWith two endpoints, browse the first and use c to copy or m to mount.\nUse : for the local side and HOST:[PATH] for the SSH side. In an attached\nshell, use origin: as the second endpoint to request approval. q quits.\n" + sshOptions,
			examples: "Browse the current directory\n  sshore lf\nBrowse a remote project and operate on it locally\n  sshore lf work:/srv/project :\nCopy a local selection to an SSH host\n  sshore lf : work:\n",
		},
		"lp": {
			run: runPortBrowser, group: groupSSH, ssh: true, max: 2,
			summary:  "Browse TCP and UDP ports and forward a service",
			help:     "Usage: sshore lp [HOST | SOURCE TARGET] [options]\n\nBrowse TCP listeners by default. Press u for UDP or t for TCP.\nThe header shows whether the browsing account is running as administrator.\nUse / to filter, r to refresh, s to switch port/process sort, and y to copy the address.\nWith two endpoints, press Enter on a listener to choose the destination port.\nUse : for the local side and HOST: for the SSH side. In an attached shell,\nuse origin: as the second endpoint to request approval. q quits.\n" + sshOptions,
			examples: "Browse services on an SSH host\n  sshore lp work\nBrowse remote services and forward one to a local port\n  sshore lp work: :\n",
		},
		"forward": {
			run: runEndpointForward, group: groupSSH, summary: "Forward a TCP or UDP port", aliases: []string{"fwd"},
			ssh: true, min: 2, max: 2, flags: []string{"--retries", "--udp"},
			help: `Usage: sshore forward SOURCE TARGET [options] [--ssh SSH_OPTIONS...]

Mirror the service at SOURCE on TARGET. Use a bare port for the current machine
and HOST:PORT for an SSH host. Use : to use the same port on the local machine.
From a remote shell, use origin: or 0: for the attached client and request local approval.
The account daemon manages the forward. Use sshore kill ID to stop it.

  sshore forward work:3000 8080
  sshore forward 8080 work:3000

Options:
  --udp          Forward UDP datagrams (default: TCP)
  --retries N    Retry lost SSH connections (0–10; default 0)

Retries cannot prompt for credentials or host keys. Existing connections
do not survive reconnect. SSH -L/-R/-D are not accepted here.
` + sshOptions,
			examples: `Expose a remote service on a local port
  sshore forward work:3000 8080
Expose a local service on a remote port
  sshore forward .:3000 work:8080
Forward a remote UDP DNS service to local port 1053
  sshore forward --udp work:53 1053
`,
		},
		"daemon": {
			run: runDaemonCommand, group: groupAccount, summary: "Run the account daemon",
			max: 1,
			help: `Usage: sshore daemon [mount]

Run the account daemon in the foreground. Clients start it automatically when needed.
The daemon owns shells and coordinates mounts and forwards.
It exits after 30 seconds without tasks or connections.
The mount subcommand runs an isolated mount worker, started by the daemon.
`,
		},
	}
	for command, spec := range cliCommands {
		for _, alias := range spec.aliases {
			commandAliases[alias] = command
		}
	}
}

func showHelp(args []string, w io.Writer) error {
	if len(args) == 0 {
		var out strings.Builder
		out.WriteString("Usage: sshore COMMAND [ARGS]\n")
		grouped := make([][]string, len(cliGroupNames))
		for command, spec := range cliCommands {
			if spec.summary != "" {
				grouped[spec.group] = append(grouped[spec.group], command)
			}
		}
		for group, names := range grouped {
			sort.Strings(names)
			fmt.Fprintf(&out, "\n%s\n", cliGroupNames[group])
			for _, name := range names {
				fmt.Fprintf(&out, "  %-11s %s\n", name, cliCommands[name].summary)
			}
		}
		out.WriteString("\n" + helpFooter)
		aliases := make([]string, 0, len(commandAliases))
		for alias, command := range commandAliases {
			aliases = append(aliases, alias+"="+command)
		}
		sort.Strings(aliases)
		out.WriteString("\nAliases: " + strings.Join(aliases, ", ") + "\n")
		_, err := io.WriteString(w, out.String())
		return err
	}
	if len(args) != 1 {
		return usage("usage: sshore help [COMMAND]")
	}
	topic := canonicalCommand(args[0])
	h := cliCommands[topic].help
	if h == "" {
		return usage("unknown help topic: %s", args[0])
	}
	_, err := io.WriteString(w, h)
	return err
}

func showTLDR(command string, w io.Writer) error {
	examples := cliCommands[canonicalCommand(command)].examples
	if examples == "" {
		return usage("no examples for %s; see sshore %s --help", command, command)
	}
	color := false
	if f, ok := w.(*os.File); ok {
		color = termui.ColorEnabled(f)
	}
	for _, line := range strings.SplitAfter(examples, "\n") {
		if strings.HasPrefix(line, "  sshore ") {
			line = termui.Format(color, termui.Info, "%s", line)
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}

func validSession(name string) error {
	if len(name) < 1 || len(name) > 48 {
		return usage("name must contain 1–48 ASCII characters")
	}
	for i, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return usage("invalid name: use letters, digits, _ and -")
		}
	}
	return nil
}

func validAlias(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if isTaskIDPrefix(name) {
		return usage("2–32 hexadecimal characters are reserved for task IDs; choose another name")
	}
	return nil
}
