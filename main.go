package main

// Command routing and argument validation live here. OpenSSH owns authentication,
// encryption, and connection configuration; sshore adds persistent Linux sessions
// and regular-file transfer without a configuration framework or database.
// bridge, daemon, sessions, and kill-session are internal commands invoked by the SSH/client
// path, not services that users need to start manually.
// SSH flags are explicitly allowlisted below; value flags accept attached or
// separate values and forwarding flags may repeat. -q belongs to sshore progress,
// -E to OpenSSH diagnostics, and -C is optional compression (which may add overhead
// on fast links or compressed data). Invalid arguments exit 2; normal remote
// shell exit preserves its code. Build with Go 1.25+; supported release targets
// are Linux/Windows amd64 only. Cross-build the Windows client with
// CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o dist/sshore.exe .

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Overridden by release builds using -ldflags '-X main.version=...'.
var version = "0.6.0"

const connectionOptions = `Options (before --; may appear before or after HOST):
  --server PATH                 Remote executable (default: PATH, then ~/.local/bin/sshore)
  --download-dir DIRECTORY      Default download directory (approval still required)
  -q, --quiet                   Hide periodic transfer progress
  --no-escape                   Forward Ctrl-] instead of detaching locally
  -h, --help                    Show command help

SSH options (only after --):
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  Pass forwarding options to OpenSSH
  -E FILE                      Write OpenSSH debug logs to FILE
  -C, -v, -vv, -vvv, -4, -6    Pass through to system OpenSSH
`

const help = `sshore — persistent shells, file transfer and SFTP mounts

Local commands:
  sshore new HOST [NAME] [options]     Create a session (default: automatic Pokemon name)
  sshore list [HOST] [-- SSH_OPTIONS]    List local resources/approvals; HOST also queries shells
  sshore cancel TYPE/NAME            Cancel a request, mount or forward
  sshore cancel HOST NAME [-- SSH_OPTIONS] End a session and its identified tasks
  sshore rename HOST OLD NEW [-- SSH_OPTIONS] Rename a running session
  sshore attach HOST [NAME] [options] Attach to an existing detached session
  sshore mount HOST [DIRECTORY [MOUNTPOINT]] Start a background file mount
  sshore forward HOST PORT from|to PORT  Start an independent background forward
  sshore review [ID] [--stay]

Inside a remote sshore shell:
  sshore detach                  Disconnect; keep the shell running
  sshore upload [DIRECTORY]      Choose a local file to upload here
  sshore download FILE...        Download one or more remote files
  sshore forward PORT from|to PORT  Request a port forward

Help:
  sshore help [COMMAND]
  sshore COMMAND --help
  sshore --version

new never resumes; attach never creates or evicts another client.
Ctrl-] detaches; exit or detected connection loss ends the session. No screen replay.
Requests for files or ports detach; run sshore review locally to review them.
Requires local OpenSSH. Session commands install missing remote software automatically.
Older remote versions offer update or recovery; newer versions reject.
Updating ends listed old sessions only after you type update. Recovery disables
persistence, transfer, mounts and dynamic forwarding until you reconnect.

Examples:
  sshore new work build
  sshore list work
  sshore attach work build
`

var commandHelp = map[string]string{
	"mount": mountHelp,
	"new": `Usage: sshore new HOST [NAME] [options]

Create a new session. Omit NAME for an automatic Pokemon name.
An existing name is an error; use sshore attach HOST NAME to resume it.
Ctrl-] or remote sshore detach leaves programs running; exit ends the shell.

` + connectionOptions,
	"attach": `Usage: sshore attach HOST [NAME] [options]

Attach to an existing detached session; never create or take over a session.
Omit NAME to attach to the only detached session, or choose from several.
Selection: #number or exact name; /text filters, / shows all.
Numbers stay unchanged after filtering. Bare numbers are session names. Enter or Ctrl-C cancels.
If no detached sessions exist, use sshore new HOST to create one.

` + connectionOptions,
	"list": `Usage: sshore list [HOST] [--details | --json] [--server PATH] [-- SSH_OPTIONS]

Without HOST, list this computer's mounts, forwards, approvals and peer inventory.
Inside an SSH shell, also list this computer's shell sessions. No remote login
is initiated just to list this computer. Peer inventory is read-only.
With HOST, filter those entries by exact SSH destination and also query shells.
--details shows worker/transport state, protocol, read-only mode, last SFTP reply,
last error and log path, including recent resource exits. --json emits one JSON
object with version, resources, sessions, peers, exits and errors. It excludes
control credentials and terminal output. A nonzero exit can accompany partial JSON.
Detailed/JSON remote inventory requires the exact release; never installs/updates.
Worker "running" means the local control endpoint answered; target health is
unchecked. Listing never probes a forwarded service. Status checks use up to eight
workers and a two-second aggregate network deadline. Receipts last seven days,
at most 100 per resource kind. Peer and session snapshots are advisory.
Stable IDs: mount/NAME, forward/NAME, approval/ID. Stop resources with sshore cancel;
review requests with sshore review ID. Expired approvals are omitted.
Peer entries use peer/KEY/RESOURCE IDs; manage them on the named computer.
Missing heartbeats become unknown after 75s; inventory is not authoritative.

The remote shell section lists session names, ages, states, foreground programs, shortened working
directories and process trees, followed by recent exit reasons.
Exit records retain at most 100 entries for seven days in the runtime directory.
Abrupt termination or runtime cleanup can leave no record.
Use sshore attach HOST NAME to resume a detached session.

Options (before --):
  -h, --help                    Show command help
  --server PATH                 Remote executable (before --)
SSH options (only after --):
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  -E FILE
  -C, -v, -vv, -vvv, -4, -6
`,
	"rename": `Usage: sshore rename HOST OLD NEW [--server PATH] [-- SSH_OPTIONS]

Rename an attached or detached session without restarting its tasks.
The old name becomes available; the new name must be unused.
Pending approvals using the old name must be requested again.
Requires matching releases; never installs or upgrades software.
SSH options are the same as sshore list; see sshore list --help.
`,
	"cancel": `Usage: sshore cancel approval/ID | mount/NAME | forward/NAME
       sshore cancel HOST NAME [--server PATH] [-- SSH_OPTIONS]

Cancel a pending request or stop a local resource shown by sshore list.
Pending requests are discarded without executing them. Mounts unmount first;
busy mounts remain active for a retry. Forwards close their own SSH connection
and all connections carried by it. Local resources require the exact release
version that started them; cancellation never kills PIDs from saved records.

End the named session and its identified tasks, including an attached session.
This takes effect immediately without confirmation; unsaved work may be lost.
NAME is required; there is no all-sessions option. Uses the current runtime
directory. Requires matching client, installed program and target daemon
release versions; never installs or upgrades software. Escaped tasks may survive.
SSH options and --server are the same as sshore list; see sshore list --help.
`,
	"review": `Usage: sshore review [ID] [--stay]

Run from your LOCAL shell after detach. Review one pending request, select
local file paths and type yes. Tab completes paths; invalid paths can be retried.
Authentication remains with system OpenSSH.
By default, approved file operations complete and the remote shell resumes.
--stay completes file operations, detaches and returns to the local shell.
A port-forward approval always starts an independent background resource and
returns locally; --stay is not applicable. The source shell stays detached.
Use sshore list / sshore cancel forward/NAME. IDs may include the approval/ prefix.
Discard a pending request with sshore cancel approval/ID.
Omit ID to select a pending request; a single request opens directly.
Selection: #number or exact ID; /text filters, / shows all. Enter or Ctrl-C cancels.
Selection only opens review; it never approves an operation. Requests expire after
10 minutes and are consumed before connecting, even when execution fails.
No SSH passwords or separate approval passwords are stored or collected.
`,
	"forward": `Usage: sshore forward HOST [CURRENT_PORT] from|to [PEER_PORT] [--name NAME] [-- SSH_OPTIONS]
       sshore forward [CURRENT_PORT] from|to [PEER_PORT]

The first port belongs to the shell running this command; the second belongs
to the other end of the SSH connection:
  A to B    Listen here on A, connect to the peer's service B.
  A from B  Listen on the peer's B, connect to the service A here.
from/to and at least one port are mandatory; a missing port copies the other.
Old PORT and colon forms are rejected. Ports must be 1..65535.

On your computer, specify HOST. Starts an independent background SSH tunnel;
--name selects its local name (default: host, direction and ports). Prints its
ID only after SSH authentication and listener setup succeed. Use sshore list
and sshore cancel forward/NAME.
--foreground keeps a local tunnel in the terminal for diagnostics; Ctrl-C stops it.  -h, --help                    Show command help

SSH options (only after --): -p PORT, -i KEY, -J JUMP, -F CONFIG, -l USER, -o OPTION, -E FILE,
-C, -v/-vv/-vvv, -4/-6. Attached short values such as -p2222 are accepted.
Explicit -L/-R/-D are not accepted here; this resource owns its forwarding rule.
--retries N (0..10, default 0) retries an established background forward with
exponential backoff (1s to 32s). It does not retry initial startup or restore
existing TCP connections. Retries cannot ask for passwords/MFA or accept new host
keys. Cancel stops retries. Not available for remote requests or --foreground.
Uses SSH configuration for keys/options. Requires a remote Unix shell and cat,
but no remote sshore installation. Server policy controls reverse listener binds.

Inside a remote sshore shell, omit HOST to request local approval. This detaches
the shell; run sshore review on your computer. Approval starts an independent
background forward and leaves the source shell detached. Stopping or exiting
that shell does not stop the forward. Management commands run on your computer.
Both ends request IPv4 127.0.0.1. No restart after reboot; reconnect is opt-in.

Examples:
  sshore forward work to 3000 --name web
  sshore forward work 3000 from 8080
  sshore forward 3000 from 8080       Remote request: computer:8080 -> server:3000
  sshore forward 8080 to 3000         Remote request: server:8080 -> computer:3000
  sshore cancel forward/web
`,
	"upload": `Usage: sshore upload [--] [DIRECTORY]

Run inside a remote sshore shell. Queue approval to upload a local file into
DIRECTORY (default: remote current directory), then detach. Run sshore review
locally to select the source (Tab completes local paths). For a single upload,
pressing Enter on the source path approves and starts it; Ctrl-C cancels.
Existing files are never overwritten.
`,
	"download": `Usage: sshore download [--] FILE...

Run inside a remote sshore shell. Queue one approval for up to 16 ordinary
files, then detach. All paths are validated before queuing. Run sshore review
locally to select destinations and execute sequentially. --download-dir only
sets destination defaults. Existing files are never overwritten. Execution
stops on the first failure; completed files remain. Helper success means queued,
not transferred; the local review command reports the execution result.
`,
	"detach": `Usage: sshore detach

Run inside a remote sshore shell. Disconnect the current client and cancel
its transfers, keeping the shell and its PID alive. Reconnect with
sshore attach HOST NAME. Ctrl-] is the local detach shortcut.
`,
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

func validSession(name string) error {
	if len(name) < 1 || len(name) > 48 {
		return usage("session name must contain 1–48 ASCII characters")
	}
	for i, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return usage("invalid session name: use letters, digits, _ and -")
		}
	}
	return nil
}

type options struct {
	host, session, server string
	command               string
	newName               string
	downloadDir           string
	list, quiet, noEscape bool
	listJSON, listDetails bool
	ssh                   []string
	approved              []operation
	approvalID            string
	resumePID             int
	stay                  bool
	plain                 bool
}

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected new, list, attach, cancel or rename; see sshore --help")
	}
	o.command = args[0]
	switch o.command {
	case "new", "attach", "cancel", "rename":
	case "list":
		o.list = true
	default:
		return o, usage("choose an action: sshore new HOST [NAME], sshore list HOST, sshore attach HOST [NAME], sshore cancel HOST NAME, or sshore rename HOST OLD NEW")
	}
	var err error
	args, o.ssh, err = splitSSHOptions(args[1:], true)
	if err != nil {
		return o, err
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			return o, usage("empty argument")
		}
		if strings.HasPrefix(a, "--") {
			if key, value, ok := strings.Cut(a, "="); ok {
				switch key {
				case "--server", "--download-dir":
				default:
					return o, usage("%s does not accept a value", key)
				}
				args = append(append(append([]string{}, args[:i]...), key, value), args[i+1:]...)
				a = key
			}
		}
		if err := misplacedSSHOption(a); err != nil {
			return o, err
		}
		switch a {
		case "--server", "--download-dir":
			i++
			if i == len(args) || args[i] == "" {
				return o, usage("missing value for %s", a)
			}
			switch a {
			case "--server":
				o.server = args[i]
			case "--download-dir":
				o.downloadDir = args[i]
			default:
				o.ssh = append(o.ssh, a, args[i])
			}
		case "--json", "--details":
			if !o.list {
				return o, usage("%s is only supported by list", a)
			}
			if a == "--json" {
				o.listJSON = true
			} else {
				o.listDetails = true
			}
		case "-q", "--quiet":
			o.quiet = true
		case "--no-escape":
			o.noEscape = true
		case "-h", "--help":
			return o, errHelp

		default:
			if o.host == "" && !strings.HasPrefix(a, "-") {
				o.host = a
			} else if o.host != "" && !o.list && o.session == "" && !strings.HasPrefix(a, "-") {
				o.session = a
			} else if o.command == "rename" && o.session != "" && o.newName == "" && !strings.HasPrefix(a, "-") {
				o.newName = a
			} else {
				return o, usage("unexpected argument: %s", a)
			}
		}
	}
	if o.listJSON && o.listDetails {
		return o, usage("--json and --details are mutually exclusive")
	}
	if o.host == "" && o.list && len(o.ssh) == 0 && o.server == "sshore" {
		if o.quiet || o.noEscape || o.downloadDir != "" {
			return o, usage("list does not accept session or transfer options")
		}
		return o, nil
	}
	if o.host == "" || strings.HasPrefix(o.host, "-") || strings.IndexFunc(o.host, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
		return o, usage("invalid SSH destination")
	}
	if o.server == "" || strings.ContainsAny(o.server, "\r\n\x00") {
		return o, usage("invalid server path")
	}
	if o.list && (o.session != "" || o.downloadDir != "" || o.quiet || o.noEscape) {
		return o, usage("list accepts SSH options and --server, not session or transfer options")
	}
	if o.list {
		return o, nil
	}
	if o.command == "rename" {
		if o.session == "" || o.newName == "" || o.downloadDir != "" || o.quiet || o.noEscape {
			return o, usage("rename requires HOST OLD NEW and accepts only SSH options and --server")
		}
		if err := validSession(o.newName); err != nil {
			return o, err
		}
	}
	if o.command == "cancel" && (o.session == "" || o.downloadDir != "" || o.quiet || o.noEscape) {
		return o, usage("cancel requires HOST NAME and accepts only SSH options and --server")
	}
	if o.session == "" {
		return o, nil
	}
	return o, validSession(o.session)
}

var errHelp = errors.New("help requested")

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
func showHelp(args []string, w io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(w, help)
		return err
	}
	if len(args) != 1 {
		return usage("usage: sshore help [COMMAND]")
	}
	h, ok := commandHelp[args[0]]
	if !ok {
		return usage("unknown help topic: %s", args[0])
	}
	_, err := io.WriteString(w, h)
	return err
}
func run(args []string) (int, error) {
	if os.Getenv("SSHORE_ASKPASS") == "1" {
		if os.Getenv("SSHORE_DISABLED") == "1" {
			return 1, fmt.Errorf("file access disabled in recovery")
		}
		return runResourceAskpass(args)
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		if _, ok := commandHelp[args[0]]; ok {
			return 0, showHelp(args[:1], os.Stdout)
		}
	}
	if os.Getenv("SSHORE_DISABLED") == "1" && len(args) > 0 {
		switch args[0] {
		case "--version", "-V", "--help", "-h", "help", "sessions", "inventory":
		default:
			return 1, fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
		}
	}
	if len(args) == 0 {
		return 0, showHelp(nil, os.Stdout)
	}
	switch args[0] {
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
	case "ftp-worker", "resource-worker":
		return runResourceWorker()
	case "mount":
		return runMount(args[1:])
	case "cancel":
		return runCancel(args[1:])
	case "list":
		if len(args) == 1 {
			return 0, listThisComputer()
		}
	case "review":
		return runReview(args[1:])
	case "forward":
		return runForward(args[1:])
	case "upload", "download", "detach":
		// -- makes filenames such as --help unambiguous.
		if len(args) > 1 && args[1] == "--" {
			args = append([]string{args[0]}, args[2:]...)
		} else {
			for _, a := range args[1:] {
				if strings.HasPrefix(a, "-") {
					return 2, usage("use %s -- %s for a path beginning with '-'", args[0], a)
				}
			}
		}
		return 0, serverCommand(args)
	case "bridge", "resume", "daemon", "sessions", "bootstrap", "kill-session", "rename-session", "task-watch", "task-remove", "inventory":
		return 0, serverCommand(args)
	}
	o, err := parseOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp(args[:1], os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	o.downloadDir, err = prepareDownloadDir(o.downloadDir)
	if err != nil {
		return 1, err
	}
	if o.list && (o.listJSON || o.listDetails) {
		return runInventory(o)
	}
	if o.list && o.host == "" {
		return 0, listThisComputer()
	}
	if o.list {
		if err = listLocalResources(o.host); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stdout, "\nRemote shell sessions on %s:\n", cleanText(o.host))
	}
	return runClient(o)
}
func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		logf(os.Stderr, logError, "sshore: %s\n", cleanText(err.Error()))
		var e *usageError
		if errors.As(err, &e) {
			code = 2
			logf(os.Stderr, logInfo, "Try 'sshore --help'.\n")
		} else if code == 0 {
			code = 1
		}
	}
	if code < 0 || code > 255 {
		code = 1
	}
	os.Exit(code)
}
