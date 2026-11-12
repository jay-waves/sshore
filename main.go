package main

// Command routing and argument validation live here. OpenSSH owns authentication,
// encryption, and connection configuration; sshore adds persistent Linux sessions
// and regular-file transfer without a configuration framework or database.
// bridge, daemon, and sessions are internal commands invoked by the SSH/client
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
var version = "0.5.3"

const connectionOptions = `Options (before or after HOST):
  --server PATH                 Remote executable (default: PATH, then ~/.local/bin/sshore)
  --download-dir DIRECTORY      Default download directory (approval still required)
  -q, --quiet                   Hide periodic transfer progress
  --no-escape                   Forward Ctrl-] instead of detaching locally
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  Pass forwarding options to OpenSSH
  -E FILE                      Write OpenSSH debug logs to FILE
  -C, -v, -vv, -vvv, -4, -6    Pass through to system OpenSSH
  -h, --help                    Show command help
`

const help = `sshore — persistent shells and file transfer over one SSH connection

Local commands:
  sshore new HOST [NAME] [options]     Create a session (default: automatic Pokemon name)
  sshore list HOST [SSH options]      List session states and process trees
  sshore attach HOST [NAME] [options] Attach to an existing detached session
  sshore approve [ID] [--attach | --stay]
  sshore approve --list
  sshore approve ID --reject

Inside a remote sshore shell:
  sshore detach                  Disconnect; keep the shell running
  sshore upload [DIRECTORY]      Choose a local file to upload here
  sshore download FILE...        Download one or more remote files
  sshore forward TARGET          Request a port forward

Help:
  sshore help [COMMAND]
  sshore COMMAND --help
  sshore --version

new never resumes; attach never creates or evicts another client.
Ctrl-] detaches; exit or detected connection loss ends the session. No screen replay.
Requests for files or ports detach; run sshore approve locally to review them.
Requires local OpenSSH. Missing remote software is installed automatically.
Older remote versions offer update or recovery; newer versions reject.
Updating ends listed old sessions only after you type update. Recovery disables
persistence, transfer and dynamic forwarding until you reconnect.

Examples:
  sshore new work build
  sshore list work
  sshore attach work build
`

var commandHelp = map[string]string{
	"new": `Usage: sshore new HOST [NAME] [options]

Create a new session. Omit NAME for an automatic Pokemon name.
An existing name is an error; use sshore attach HOST NAME to resume it.
Ctrl-] or remote sshore detach leaves programs running; exit ends the shell.

` + connectionOptions,
	"attach": `Usage: sshore attach HOST [NAME] [options]

Attach to an existing detached session; never create or take over a session.
Omit NAME to attach to the only detached session, or choose from several.
If no detached sessions exist, use sshore new HOST to create one.

` + connectionOptions,
	"list": `Usage: sshore list HOST [SSH options] [--server PATH]

List session names, attached/detached states and process trees.
Use sshore attach HOST NAME to resume a detached session.

SSH options:
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION
  -L SPEC  -R SPEC  -D ADDRESS  -E FILE
  -C, -v, -vv, -vvv, -4, -6
  --server PATH                 Remote executable
  -h, --help                    Show command help
`,
	"approve": `Usage: sshore approve [ID] [--attach | --stay]
       sshore approve --list
       sshore approve ID --reject

Run from your LOCAL shell after detach. Review one pending request, select
local file paths and type yes. Authentication remains with system OpenSSH.
--attach (default) executes approved operations and resumes the remote shell.
--stay completes file operations, detaches and returns to the local shell.
For a port forward it runs a foreground tunnel without remote terminal I/O;
Ctrl-C closes the tunnel and ends the remote session.
Omit ID only when exactly one unexpired request exists. Requests expire after
10 minutes and are consumed before connecting, even when execution fails.
No SSH passwords or separate approval passwords are stored or collected.
`,
	"forward": `Usage: sshore forward TARGET | --stop PORT | --list

Run inside a remote sshore shell. TARGET is PORT, LOCAL_PORT:REMOTE_PORT, or
LOCAL_PORT:REMOTE_HOST:REMOTE_PORT. Adding a rule queues local approval and
detaches; run sshore approve locally to open 127.0.0.1:LOCAL_PORT to the
remote target. --list and --stop do not require approval. Stop/detach closes
existing connections. OpenSSH -L rules are managed separately.
`,
	"upload": `Usage: sshore upload [--] [DIRECTORY]

Run inside a remote sshore shell. Queue approval to upload a local file into
DIRECTORY (default: remote current directory), then detach. Run sshore approve
locally to select the source (Tab completes local paths). For a single upload,
pressing Enter on the source path approves and starts it; Ctrl-C cancels.
Existing files are never overwritten.
`,
	"download": `Usage: sshore download [--] FILE...

Run inside a remote sshore shell. Queue one approval for up to 16 ordinary
files, then detach. All paths are validated before queuing. Run sshore approve
locally to select destinations and execute sequentially. --download-dir only
sets destination defaults. Existing files are never overwritten. Execution
stops on the first failure; completed files remain. Helper success means queued,
not transferred; the local approve command reports the execution result.
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
	downloadDir           string
	list, quiet, noEscape bool
	ssh                   []string
	approved              []operation
	resumePID             int
	stay                  bool
	plain                 bool
}

func parseOptions(args []string) (options, error) {
	o := options{server: "sshore"}
	if len(args) == 0 {
		return o, usage("expected new, list or attach; see sshore --help")
	}
	o.command = args[0]
	switch o.command {
	case "new", "attach":
	case "list":
		o.list = true
	default:
		return o, usage("choose an action: sshore new HOST [NAME], sshore list HOST, or sshore attach HOST [NAME]")
	}
	args = args[1:]
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
		if len(a) > 2 && a[0] == '-' && strings.ContainsRune("piJFloLRDE", rune(a[1])) {
			o.ssh = append(o.ssh, a[:2], a[2:])
			continue
		}
		switch a {
		case "--server", "--download-dir", "-p", "-i", "-J", "-F", "-l", "-o", "-L", "-R", "-D", "-E":
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
		case "-q", "--quiet":
			o.quiet = true
		case "--no-escape":
			o.noEscape = true
		case "-C", "-v", "-vv", "-vvv", "-4", "-6":
			o.ssh = append(o.ssh, a)
		case "-h", "--help":
			return o, errHelp
		case "--":
			i++
			if i == len(args) {
				return o, usage("missing destination or name after --")
			}
			for ; i < len(args); i++ {
				if args[i] == "" {
					return o, usage("empty argument")
				}
				if o.host == "" {
					o.host = args[i]
				} else if !o.list && o.session == "" {
					o.session = args[i]
				} else {
					return o, usage("unexpected argument: %s", args[i])
				}
			}
		default:
			if o.host == "" && !strings.HasPrefix(a, "-") {
				o.host = a
			} else if o.host != "" && !o.list && o.session == "" && !strings.HasPrefix(a, "-") {
				o.session = a
			} else {
				return o, usage("unexpected argument: %s", a)
			}
		}
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
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		if _, ok := commandHelp[args[0]]; ok {
			return 0, showHelp(args[:1], os.Stdout)
		}
	}
	if os.Getenv("SSHORE_DISABLED") == "1" && len(args) > 0 {
		switch args[0] {
		case "--version", "-V", "--help", "-h", "help", "sessions":
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
	case "approve":
		return runApprove(args[1:])
	case "forward":
		if _, err := parseForward(args[1:]); err != nil {
			return 2, err
		}
		return 0, serverCommand(args)
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
	case "bridge", "resume", "daemon", "sessions", "bootstrap":
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
