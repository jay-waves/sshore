package main

// Command routing and argument validation live here. OpenSSH owns authentication,
// encryption, and connection configuration; sshore adds persistent Linux shells
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

	"github.com/jay-waves/sshore/internal/termui"
)

// Overridden by release builds using -ldflags '-X main.version=...'.
var version = "0.6.7"

const connectionOptions = `Options (before --; may appear before or after HOST):
  --server PATH                 Remote executable (default: PATH, then ~/.local/bin/sshore)
  --download-dir DIRECTORY      Default download directory (approval still required)
  -q, --quiet                   Hide periodic transfer progress
  --no-escape                   Forward all input; disable local mux shortcuts
  -h, --help                    Show command help

Ctrl+B then ? shows session shortcuts.

SSH options (only after --):
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  Pass forwarding options to OpenSSH
  -E FILE                      Write OpenSSH debug logs to FILE
  -C, -v, -vv, -vvv, -4, -6    Pass through to system OpenSSH
`

const help = `sshore — persistent shells, file transfer and SFTP mounts

Local commands:
  sshore shell [HOST [NAME]] [options]   Create a persistent local or remote shell
  sshore spawn [HOST [NAME]] [options]   Create a shell bound to the launching parent shell
  sshore list [HOST] [-- SSH_OPTIONS]    Local records, or query the named remote account
  sshore cancel TYPE/NAME            Cancel a request, mount or forward
  sshore cancel HOST NAME [-- SSH_OPTIONS] End a shell and its identified tasks
  sshore rename HOST OLD NEW [-- SSH_OPTIONS] Rename a running shell
  sshore attach [HOST [NAME]] [options] Attach to an existing detached shell
  sshore mount HOST [DIRECTORY [MOUNTPOINT]] Start a background file mount
  sshore cp SOURCE... TARGET          Copy files through SSH
  sshore forward LISTENER SERVICE     Start an independent background forward
  sshore review [NAME] [--stay]

Inside a remote sshore shell:
  sshore detach                  Disconnect; keep the shell running
  sshore cp FILE... origin:       Choose local destinations after detach
  sshore cp origin: DIRECTORY     Choose a local source after detach
  sshore forward origin: PORT     Choose a local listener after detach
  sshore forward PORT origin:     Choose a local service after detach

Help:
  sshore help [COMMAND]
  sshore COMMAND --help
  sshore --version

shell never resumes; attach never creates or evicts another client.
Ctrl+B then d detaches; Ctrl+B then ? shows keys.
Exit ends the shell. shell survives connection loss; spawn ends with its owner
or its lifetime SSH connection. No screen replay.
Ctrl+B keys: c new session; n/p next/previous; l last; s/w choose session.
            d detach; r redraw; i info; ? help; Ctrl+B send prefix.
Unknown prefix combinations pass through. --no-escape disables all local shortcuts.
Requests for files or ports detach; run sshore review locally to review them.
Requires local OpenSSH. New/attach commands install missing remote software automatically.
Older remote versions offer update or recovery; newer versions reject.
Updating ends listed old shells only after you type update. Recovery disables
persistence, transfer, mounts and dynamic forwarding until you reconnect.

Examples:
  sshore shell work build
  sshore list work
  sshore attach work build
`

var commandHelp = map[string]string{
	"mount": mountHelp,
	"spawn": `Usage: sshore spawn [HOST [NAME]] [options]
       sshore spawn [--local] [--name NAME] [--detach] [--quiet] [--no-escape]

Create a temporary shell bound to the parent shell that launches this command.
Omit HOST for local use. SHELL selects the executable; on Windows, fall back
to COMSPEC, then cmd.exe. SHELL is a path, without arguments.
Ctrl+B d returns to the parent shell; attach resumes the background spawn.
--detach starts in the background. Neither detach nor attach changes its owner.
Closing the original parent shell ends the spawn and its identified tasks.
A remote spawn also ends when its lifetime SSH connection is lost; detection
may take up to the transport timeout. There is no automatic reconnect.
Ctrl+B c creates another spawn bound to this client's parent shell.
list distinguishes spawn/NAME from persistent shell/NAME; numeric IDs work for both.
Use sshore shell for persistence beyond the launching terminal.
` + connectionOptions,
	"shell": `Usage: sshore shell [HOST [NAME]] [options]
       sshore shell [--local] [--name NAME] [--quiet] [--no-escape]

Create a local shell when HOST is omitted; preserve the current directory.
Use SHELL as the executable path. Windows falls back to COMSPEC, then cmd.exe.
Use --name to name a local shell. With HOST, create a remote shell over SSH.
Omit NAME for an automatic Pokemon name. --detach creates without attaching.
An existing name is an error; use sshore attach HOST NAME to resume it.
Ctrl+B d, terminal closure or connection loss leaves programs running.
Exit ends the shell; sshore detach also works inside a remote shell.

` + connectionOptions,
	"attach": `Usage: sshore attach [HOST [NAME]] [options]
       sshore attach ID
       sshore attach --local [NAME]

Attach to an existing detached shell; never create or take over a shell.
A bare numeric argument selects a stable task ID from sshore list.
--local selects a local shell; omit NAME when exactly one is detached.
Omit HOST and NAME to reuse the most recent successful shell connection.
With HOST only, reuse its most recent shell; without history, select a detached shell.
Remembered connections include --server and SSH transport options, never typed passwords.
Explicit --server or SSH options override the remembered values. Native forwarding
flags are not remembered. A missing or attached remembered shell is an error;
use sshore list HOST and specify a different NAME.
Selection: #number or exact name; /text filters, / shows all.
Numbers stay unchanged after filtering. Bare numbers are shell names. Enter or Ctrl-C cancels.
If no detached shells exist, use sshore shell HOST to create one.

` + connectionOptions,
	"list": `Usage: sshore list [HOST] [--verbose | --json] [--server PATH] [-- SSH_OPTIONS]

Without HOST, show local connections, resources, pending reviews and known shell
history ordered by stable numeric task ID. Remote hosts are not queried and SSH authentication is
never triggered. This is not a complete remote task list. Cached states show their
last confirmation time; loss without detach acknowledgement is unconfirmed.
A confirmed detach is recorded immediately. Other computers' actions and remote
exits require a later explicit query.
With HOST, announce and query that remote account over SSH. No local resource
table is mixed into the remote result. All remote queries require the exact
release and never install or update software. Query failures are reported.
--verbose adds remote process trees, foreground programs, directories and detailed
resource state, including recent exits. --json emits version, host,
remote_queried, resources, shells, peers, exits and errors. Local shell records
include historical, confirmed_at and local_connection_active. No control tokens
or terminal output are exported; partial results may have a nonzero exit status.
Worker running confirms the local worker, not remote service health. Targets are
never probed. Resource status checks have a two-second aggregate deadline.
Numeric task IDs are stable, increasing and never reused; gaps are normal.
Use attach ID, cancel ID, rename ID NEW, or review ID. Cached tasks are revalidated
before execution. A task that ended is never replaced by a same-name task.
--local lists only local shells. Names: shell/NAME, spawn/NAME, mount/NAME, forward/NAME,
review/NAME. Names share a local namespace
and default to Pokemon names; explicit names remain supported. Use cancel
mount/NAME or forward/NAME locally, cancel HOST NAME for a remote shell, and
review NAME to inspect a request. Peer entries are read-only and name their
managing computer; missing heartbeats become unknown after 75 seconds.

Options (before --):
  -h, --help                    Show command help
  --server PATH                 Remote executable (before --)
SSH options (only after --):
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  -E FILE
  -C, -v, -vv, -vvv, -4, -6
`,
	"rename": `Usage: sshore rename HOST OLD NEW [--server PATH] [-- SSH_OPTIONS]
       sshore rename ID NEW
       sshore rename --local OLD NEW

Rename an attached or detached shell without restarting its tasks.
The old name becomes available; the new name must be unused.
Pending approvals using the old name must be requested again.
Requires matching releases; never installs or upgrades software.
SSH options are the same as sshore list; see sshore list --help.
`,
	"cancel": `Usage: sshore cancel ID
       sshore cancel --local NAME
       sshore cancel review/NAME | mount/NAME | forward/NAME
       sshore cancel HOST NAME [--server PATH] [-- SSH_OPTIONS]
       sshore cancel [HOST] --all [--server PATH] [-- SSH_OPTIONS]

--all shows the current tasks and requires typing cancel before stopping them.
With HOST, include that account's shells/resources and local resources connected
to HOST. Without HOST, stop local shells/resources only; cached remote shells and
other computers' read-only tasks are excluded. Tasks created later are not stopped.

Cancel a pending request or stop a local resource shown by sshore list.
Pending requests are discarded without executing them. Mounts unmount first;
busy mounts remain active for a retry. Forwards close their own SSH connection
and all connections carried by it. Local resources require the exact release
version that started them; cancellation never kills PIDs from saved records.

End the named shell and its identified tasks, including an attached shell.
This takes effect immediately without confirmation; unsaved work may be lost.
A named shell is resolved in the current runtime directory; confirmed task IDs
and --all cover visible account shells. Requires matching client, installed program and target daemon
release versions; never installs or upgrades software. Escaped tasks may survive.
SSH options and --server are the same as sshore list; see sshore list --help.
`,
	"review": `Usage: sshore review [NAME] [--stay]

Run from your LOCAL shell after detach. Review one pending request, select
local file paths or a local port. Submitting all required valid inputs approves
and starts execution; there is no additional yes prompt. Ctrl-C cancels.
Tab completes paths; invalid paths and ports can be retried.
Authentication remains with system OpenSSH.
By default, approved file operations complete and the remote shell resumes.
--stay completes file operations, detaches and returns to the local shell.
A port-forward approval always starts an independent background resource and
returns locally; --stay is not applicable. The source shell stays detached.
Approved forwards retain the review name: review/NAME becomes forward/NAME.
Use sshore list / sshore cancel forward/NAME. Names may include the review/ prefix.
Discard a pending request with sshore cancel review/NAME.
Omit NAME to select a pending request; a single request opens directly.
Selection: #number or exact name; /text filters, / shows all. Enter or Ctrl-C cancels.
Selection only opens review; it never approves an operation. Requests expire after
10 minutes and are consumed before connecting, even when execution fails.
No SSH passwords or separate approval passwords are stored or collected.
`,
	"cp":      copyHelp,
	"forward": forwardHelp,
	"detach": `Usage: sshore detach

Run inside a remote sshore shell. Disconnect the current client and cancel
its transfers, keeping the shell and its PID alive. Reconnect with
sshore attach HOST NAME. Ctrl+B d detach locally.
`,
}

type usageError struct{ message string }

func (e *usageError) Error() string          { return e.message }
func usage(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

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

type options struct {
	temporary             bool
	owner                 spawnOwner
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
	local                 bool
	detached              bool
	taskIdentity          string
	instance              string
	serverSet             bool
	sshDirectory          string
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
	case "cancel-resource":
		if len(args) != 4 {
			return 2, usage("invalid resource cancellation")
		}
		if err := checkPeerVersion(args[1], "client"); err != nil {
			return 1, err
		}
		return 0, cancelResourceIdentity(args[2], args[3])
	case "local-daemon":
		return 0, runLocalDaemon()
	case "shell", "spawn", "attach", "rename":
		return runShellCommand(args)
	case "ftp-worker", "resource-worker":
		return runResourceWorker()
	case "mount":
		return runMount(args[1:])
	case "cancel":
		return runCancel(args[1:])
	case "list":
		return runShellCommand(args)
	case "review":
		if code, handled, err := runTaskID(args); handled {
			return code, err
		}
		return runReview(args[1:])
	case "forward":
		return runEndpointForward(args[1:])
	case "cp":
		return runCopy(args[1:])
	case "detach":
		if len(args) != 1 {
			return 2, usage("detach takes no arguments")
		}
		if identity := os.Getenv("SSHORE_LOCAL_SESSION"); identity != "" {
			return 0, detachLocalShell(identity)
		}
		return 0, serverCommand(args)
	case "forward-watch", "mount-agent", "bridge", "resume", "daemon", "sessions", "bootstrap", "kill-session", "rename-session", "task-watch", "task-remove", "inventory":
		return 0, serverCommand(args)
	}
	return 2, usage("unknown command: %s", args[0])
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		termui.Log(os.Stderr, termui.Error, "sshore: %s\n", cleanText(err.Error()))
		var e *usageError
		if errors.As(err, &e) {
			code = 2
			termui.Log(os.Stderr, termui.Info, "Try 'sshore --help'.\n")
		} else if code == 0 {
			code = 1
		}
	}
	if code < 0 || code > 255 {
		code = 1
	}
	os.Exit(code)
}
