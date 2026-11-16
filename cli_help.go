package main

import "io"

const connectionOptions = `Options (before --; may appear before or after HOST; --option=VALUE is accepted):
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
  sshore launch [HOST [NAME]] [options]   Create a persistent local or remote shell
  sshore spawn [HOST [NAME]] [options]   Create a shell bound to the launching parent shell
  sshore pstree                     Show shell ownership and attachment sources
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
  sshore completion SHELL       Print shell completion registration

launch never resumes; attach never creates or evicts another client.
Ctrl+B then d detaches; Ctrl+B then ? shows keys.
Exit ends the shell. launch survives connection loss; spawn ends with its owner
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
  sshore launch work build
  sshore list work
  sshore attach work build
`

var commandHelp = map[string]string{
	"completion": `Usage: sshore completion SHELL

Print registration for Bash, Zsh, Fish or PowerShell 7+. Load it in your shell:
  Bash:       source <(sshore completion bash)
  Zsh:        source <(sshore completion zsh)   # after compinit
  Fish:       sshore completion fish | source
  PowerShell: sshore completion powershell | Out-String | Invoke-Expression

Add the matching line to your shell's startup file to enable it persistently.
No startup files are changed automatically. cmd.exe has no supported adapter.
Completion uses command definitions, literal SSH Host aliases (including bounded
Includes), recent hosts and local/cached task records. Candidates can be stale;
commands still revalidate them. It never connects, authenticates, starts daemons,
allocates task IDs, modifies state or installs software. Remote paths are not
queried. File and directory completion uses your shell's native facilities.
`,
	"pstree": `Usage: sshore pstree [HOST] [-- SSH_OPTIONS]

Show shell ownership and attachment sources, with the current shell highlighted.
Branches show the original spawn owner context; attach via shows interaction only.
Detach/reattach never changes lifetime or ownership. Unknown owners appear at root.
Confirmed ended shells are omitted; surviving children remain visible.
HOST groups ownership roots; @HOST marks a change of host along a branch.
The header attached label is the default for nodes with no explicit state; it
does not infer ancestor liveness. A header cached label applies to the whole tree;
otherwise cached nodes are labelled individually. Unknown state stays explicit.
launch/ denotes persistent shells; spawn/ denotes temporary shells bound to their
owner (local parent or remote heartbeat).
Cross-host relationships are display snapshots, not verified OS process ancestry.
NO_COLOR disables terminal colors.
`,
	"mount": mountHelp,
	"spawn": `Usage: sshore spawn [HOST [NAME]] [options]
       sshore spawn --local [NAME] [--detach] [--no-login] [--no-profile] [--quiet] [--no-escape]

Create a temporary shell bound to the parent shell that launches this command.
Omit HOST for local use. SHELL selects the executable; on Windows, fall back
to COMSPEC, then cmd.exe. SHELL is a path, without arguments.
Linux starts bash/zsh/fish/sh/dash/ash/ksh as a login shell by default.
--no-login selects ordinary interactive startup; use it for other Linux shells.
--no-profile skips optional startup files for bash/sh/zsh/fish and implies
non-login startup on Linux. For sh, it also removes ENV from the child environment.
Windows: --no-profile uses PowerShell -NoProfile or CMD /D; --no-login is rejected.
Inherited environment variables remain. Zsh still reads the system zshenv.
Startup options apply only to new shells; startup files are owned by the shell.
Ctrl+B d returns to the parent shell; attach resumes the background spawn.
--detach starts in the background. Neither detach nor attach changes its owner
or lifetime. Remote detach keeps the background lifetime heartbeat running.
Closing the original parent shell ends the spawn and its identified tasks.
A remote spawn also ends when its lifetime SSH connection is lost; detection
may take up to the transport timeout. There is no automatic reconnect.
Ctrl+B c creates another spawn bound to this client's parent shell.
list shows LIFETIME: parent (local spawn), heartbeat (remote spawn), or persistent
(launch). These are lifetime policies, not live heartbeat health readings.
Numeric IDs work for both spawn/NAME and launch/NAME.
Use sshore launch for persistence beyond the launching terminal.
` + connectionOptions,
	"launch": `Usage: sshore launch [HOST [NAME]] [options]
       sshore launch --local [NAME] [--detach] [--no-login] [--no-profile] [--quiet] [--no-escape]

Create a local shell when HOST is omitted; preserve the current directory.
Use SHELL as the executable path. Windows falls back to COMSPEC, then cmd.exe.
Use --local NAME to name a local shell. With HOST, create a remote shell over SSH.
Omit NAME for an automatic Pokemon name. --detach creates without attaching.
Linux starts bash/zsh/fish/sh/dash/ash/ksh as a login shell by default.
--no-login selects ordinary interactive startup; use it for other Linux shells.
--no-profile skips optional startup files for bash/sh/zsh/fish and implies
non-login startup on Linux. For sh, it also removes ENV from the child environment.
Windows: --no-profile uses PowerShell -NoProfile or CMD /D; --no-login is rejected.
Inherited environment variables remain. Zsh still reads the system zshenv.
Startup options apply only to new shells; startup files are owned by the shell.
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
If no detached shells exist, use sshore launch HOST to create one.

` + connectionOptions,
	"list": `Usage: sshore list [HOST] [--verbose | --json] [--server PATH] [-- SSH_OPTIONS]

Without HOST, show local connections, resources, pending reviews and known shell
history ordered by stable numeric task ID. Remote hosts are not queried and SSH authentication is
never triggered. This is not a complete remote task list. CHECKED shows when a
state was last confirmed, on the HOST line when shared or in a column when times
differ. Loss without detach acknowledgement is unconfirmed.
A confirmed detach is recorded immediately. Other computers' actions and remote
exits require a later explicit query. Confirmed absent or exited cached shells
are hidden by default; --verbose and --json include recent shell history. Ended
shell history expires after seven days; failed or incomplete queries do not
confirm disappearance. Use cancel to clear ended history; see sshore cancel --help.
Recent connection shortcuts do not create task records.
With HOST, announce and query that remote account over SSH. No local resource
table is mixed into the remote result. All remote queries require the exact
release and never install or update software. Query failures are reported.
--verbose adds LIFETIME, remote process trees, foreground programs, directories
and detailed resource state, including recent exits. --json emits version, host,
remote_queried, resources, shells, peers, exits and errors. Local shell records
include historical, confirmed_at and local_connection_active. No control tokens
or terminal output are exported; partial results may have a nonzero exit status.
HOST groups identify where shells run and where mount/forward processes are managed.
Local sessions appear first. REMOTE names a resource's connection target; shells
show -. Peer records appear under their managing computer and remain read-only.
CHECKED does not imply continuous liveness; - means no check time is available
for this entry. NO_COLOR disables terminal colors; redirected output is plain.
LIFETIME (lifetime in shell JSON) is fixed at startup: parent for local spawn,
heartbeat for remote spawn, persistent for launch. Detach/attach never changes
it. This is policy, not a live heartbeat health check. Use pstree for owner
contexts and attachment sources.
Unknown peer paths or ports are not inferred from descriptions. --verbose adds
paths, descriptions and full peer identities.
Worker running confirms the worker, not remote service health. Targets are
never probed. Resource status checks have a two-second aggregate deadline.
Numeric task IDs are stable, increasing and never reused; gaps are normal.
Use attach ID, cancel ID, rename ID NEW, or review ID. Cached tasks are revalidated
before execution. A task that ended is never replaced by a same-name task.
--local lists only local shells. Names: launch/NAME, spawn/NAME, mount/NAME, forward/NAME,
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
  -E FILE                      Write OpenSSH debug logs to FILE
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
The local background manager exits as soon as its remaining tasks finish.

Cancel a pending request or stop a local resource shown by sshore list.
Pending requests are discarded without executing them. Mounts unmount first;
busy mounts remain active for a retry. Forwards close their own SSH connection
and all connections carried by it. Local resources require the exact release
version that started them; cancellation never kills PIDs from saved records.

For confirmed exited/not-listed remote history, cancel ID or cancel HOST NAME
clears the local record without SSH or process signals. Repeated cleanup succeeds
while that history is retained. Other shell states still require remote checks.

End the named shell and its identified tasks, including an attached shell.
This takes effect immediately without confirmation; unsaved work may be lost.
A named shell is resolved in the current runtime directory; confirmed task IDs
and --all cover visible account shells. Requires matching client, installed program and target daemon
release versions; never installs or upgrades software. Escaped tasks may survive.
SSH options and --server are the same as sshore list; see sshore list --help.
`,
	"review": `Usage: sshore review [NAME] [--stay]

Run from your LOCAL shell after detach. Review one pending request, select
local file paths, a mountpoint or a local port. Submitting all required valid inputs approves
and starts execution; there is no additional yes prompt. Ctrl-C cancels.
Tab completes paths; invalid paths and ports can be retried.
Authentication remains with system OpenSSH.
By default, approved file operations complete and the remote shell resumes.
--stay completes file operations, detaches and returns to the local shell.
A mount or port-forward approval starts an independent background resource and
returns locally; --stay is not applicable. The source shell stays detached.
Approved resources retain the review name: review/NAME becomes mount/NAME or
forward/NAME. Use sshore list / sshore cancel TYPE/NAME. Names may include the review/ prefix.
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

const mountHelp = `Usage: sshore mount HOST [REMOTE_DIRECTORY [MOUNTPOINT]] [options] [-- SSH_OPTIONS]
       sshore mount DIRECTORY origin:[MOUNTPOINT] [--read-only]   (remote shell)

Mount a remote directory for local applications using SFTP (not plain FTP).
Starts a named local background mount after SSH authentication succeeds.
Inside a remote sshore shell, mount DIRECTORY origin: requests a mount on the
attached client and detaches. Run review locally to select a mountpoint; Enter
uses the automatic default when no path was suggested. An origin:PATH is only
an editable suggestion. Review shows read-write/read-only access and starts a
background mount, leaving the source shell detached. Only --read-only is accepted
on remote requests; SSH settings come from the original connection. The source
shell must still exist with the same name/PID, matching version and no attachment.
Linux custom mountpoints must be existing empty directories; Windows accepts an
unused drive letter or nonexistent directory. Cancel before submitting to defer.
Defaults: remote home directory; ~/mnt/NAME on Linux, free drive on Windows.
Use sshore list to find mount paths and sshore cancel mount/NAME to unmount.
With --foreground, Ctrl-C requests a graceful unmount (10s grace).
A second Ctrl-C while waiting forces disconnect and can interrupt writes.
A busy mount stays connected; close files and retry.
An SFTP request timeout closes the entire mount transport, including sibling
operations; writes may be incomplete or already completed and are never retried.
Idle mounts have no request timer. Startup timeout is opt-in because it includes
SSH password/MFA entry; use SSH ConnectTimeout for network connection setup. Uses a separate SSH connection.
Default: matching remote sshore and an OpenSSH sftp-server executable; file
notifications share this SSH connection. Missing remote sshore installs automatically;
older installations require typed update confirmation and may end listed shells.
Failed remote downloads fall back to a verified local download/cache over the same SSH connection.
Use --no-watch for a plain SFTP subsystem without remote sshore.
Requires WinFsp on Windows or FUSE on Linux. Missing WinFsp prints its install URL.
No persistent write-back cache. Background mounts publish remote inventory only;
local paths/control tokens are not shared. Sync failure does not fail the mount.

Options:
  --name NAME                   Resource name (default: automatic Pokemon name)
  --foreground                  Keep this mount in the terminal
  --no-watch                    Use plain SFTP without remote file notifications
  --server PATH                 Remote sshore executable (requires notifications; default: PATH, then ~/.local/bin/sshore)
  --read-only                   Reject changes through this mount
  --io-timeout DURATION         Per SFTP request (default 60s; 0s disables)
  --startup-timeout DURATION    Total startup including authentication (default 0s)
  --stats                       Emit cumulative request/cache/callback statistics every 10s and at exit
                                Foreground: stderr; background: worker log. No paths or contents.
  --direct-io                   Linux: disable kernel file read caching
  -h, --help                    Show command help

SSH options (only after --):
  -p PORT  -i KEY  -J JUMP  -F CONFIG  -l USER  -o OPTION  -E FILE
  -C, -v, -vv, -vvv, -4, -6    Pass through to system OpenSSH
  --                            Begin SSH-only options

Examples:
  sshore mount work --name work
  sshore cancel mount/work
  sshore mount work /home/alice/project ./remote-project
  sshore mount work /home/alice/project X:
`

const copyHelp = `Usage: sshore cp SOURCE... TARGET [-- SSH_OPTIONS]

On your computer, use HOST:PATH for the remote side and a local path for the
other. HOST is an SSH alias or user@host; bracket IPv6. Relative remote paths
start in the remote account's login directory. An empty remote path means ".".
origin:PATH explicitly names a local path. Use ./ to disambiguate filenames with
a colon or leading hyphen. On Windows, drive paths are local and take precedence
over single-letter SSH aliases; use user@HOST:PATH to disambiguate a remote host.
A bare - is stdin in SOURCE position or stdout in TARGET position, for local
commands with exactly one remote file. Stdin requires a new destination filename,
not a directory; EOF completes the upload. Data streams directly without local
spooling. Stdout contains only file bytes; errors go to stderr. A failed stdout
copy may already have emitted partial data. Use ./- or origin:- for a file named -.
Standard streams are not available in remote approval requests.
At most 16 regular files; no symlinks, recursive directories, overwrites, metadata
copying, local-only or remote-to-remote copies. Multiple sources require a target
directory. A single target may be a directory or a new filename.
Local copies use an independent connection and require matching remote sshore.
Missing software is installed through helper setup; updating older versions requires
typing update and ends the listed shells/tasks. No persistent shell, FUSE or WinFsp
is required. Uploads are verified on the receiver without downloading them again.
File destinations must support hard links.

origin: and : are equivalent placeholders for a locally selected path.
On your computer, they open a local selector. Inside a remote sshore shell,
exactly one side must name origin: (or :): cp origin: DIRECTORY requests an upload;
cp FILE... origin: requests downloads. A filled origin:PATH is an editable local
review suggestion; Enter accepts it. Remote requests detach; run sshore review
locally. origin means the current connection's client, never the invoking shell.
A suggested path never skips review. Relative local hints use the review working
directory. The remote upload target must be a directory.
Tab completes paths. Submitting all valid paths starts copying without an extra
yes prompt. Ctrl-C during selection cancels before any copy starts.
Batches execute sequentially; a failure keeps completed files and stops the rest.
Interrupted copies remove temporary files when possible; forced termination may
leave .sshore-*.part files. Files are published atomically without overwriting.

SSH options apply only to local direct copies, and only after --:
-p PORT, -i KEY, -J JUMP, -F CONFIG, -l USER, -o OPTION, -E FILE,
-C, -v/-vv/-vvv, -4/-6. -L/-R/-D are not accepted.
-h, --help shows this help.

Examples:
  sshore cp work:/tmp/report.txt - | less
  generate-report | sshore cp - work:/tmp/report.txt
  sshore cp report.zip work:/tmp/
  sshore cp work:/tmp/report.zip ./downloads/
  sshore cp a.zip b.zip work:/tmp/
  sshore cp report.zip origin:        Remote request: choose a local destination
  sshore cp : .                      Remote request: choose a local source
`
const forwardHelp = `Usage: sshore forward LISTENER SERVICE [options] [-- SSH_OPTIONS]

Connections enter LISTENER and are forwarded to SERVICE.
On your computer, use HOST:PORT for exactly one endpoint and origin:PORT (or a
bare PORT) for the other. HOST is an SSH alias or user@host; bracket IPv6.
Use origin: or : instead of the local endpoint to choose its port interactively.
Use -- -p PORT for the SSH login port; endpoint ports are service/listener ports.

Inside a remote sshore shell, use a bare server port and one origin:[PORT]
(or :) endpoint. This saves a request and detaches. Run sshore review on your
computer to select the local port. origin always means that connection's client,
even when the command runs on the server. A filled local port is an editable review suggestion. A valid submitted port
approves and starts execution;
there is no extra yes prompt. Ctrl-C cancels; Enter accepts a displayed suggestion.
Without a suggestion, empty input retries; invalid ports always retry.

An independent background resource owns each forward. The source shell stays
detached after approval. Manage with sshore list / sshore cancel forward/NAME.
Both endpoints use IPv4 loopback; SSH server policy can override reverse binds.

Local options:
  --name NAME        Resource name (default: automatic Pokemon name)
  --foreground       Run in this terminal; Ctrl-C stops forwarding
  --retries N        Retry established background connections 0..10 times
                     (default 0; not initial startup or existing connections)
  -h, --help         Show help
Retries cannot ask for credentials or accept new host keys. No reboot restart.
SSH options after --: -p PORT, -i KEY, -J JUMP, -F CONFIG, -l USER, -o OPTION,
-E FILE, -C, -v/-vv/-vvv, -4/-6. Native -L/-R/-D are not accepted here.
Local creation needs a remote Unix shell and cat, not remote sshore installation.

Examples:
  sshore forward origin:8080 work:3000 --name web
  sshore forward work:3000 origin:8080
  sshore forward origin: 3000          Remote request: client listener -> server service
  sshore forward 3000 :               Remote request: server listener -> client service
`

func showHelp(args []string, w io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(w, help)
		return err
	}
	if len(args) != 1 {
		return usage("usage: sshore help [COMMAND]")
	}
	topic := args[0]
	h, ok := commandHelp[topic]
	if !ok {
		return usage("unknown help topic: %s", args[0])
	}
	_, err := io.WriteString(w, h)
	return err
}
