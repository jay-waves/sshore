package main

import (
	"io"
	"sort"
	"strings"
)

const help = `Usage: sshore COMMAND [ARGS]

Shells
  launch      Start a persistent shell
  spawn       Start a shell that ends with its owner
  attach      Resume a detached shell
  detach      Leave the current shell running
  rename      Rename a running shell

Files and ports
  cp          Copy files over SSH
  mount       Mount a remote directory
  forward     Forward a TCP port
  review      Review requests from a remote shell

Tasks and setup
  list        Show tasks and when they were last checked
  pstree      Show shell ownership and attachments
  cancel      Stop tasks or discard requests
  reset       Replace local identity and clear history
  completion  Set up shell completion

Start: sshore launch work build     Resume: sshore attach work build
In a session: Ctrl+B d detaches; Ctrl+B ? shows shortcuts.
Details: sshore COMMAND --help      Version: sshore --version
`

const sshOptions = `
SSH options follow --, for example: -- -p 2222 -J jump.
Supported: -p -i -J -F -l -o -E -C -v/-vv/-vvv -4 -6.
`

const connectionOptions = `  --server PATH         Remote executable (default: PATH, then ~/.local/bin/sshore)
  --download-dir DIR    Default download directory; review still required
  -q, --quiet           Hide periodic transfer progress
  --no-escape           Disable Ctrl+B shortcuts
`

const shellStartupOptions = `  --local               Select a local shell
  --detach              Start without attaching
  --no-login            Use non-login startup (Linux)
  --no-profile          Skip optional shell startup files; implies --no-login on Linux
`

const shellConnectionHelp = `
Missing remote software is installed automatically when needed. Updating an
older release can end the listed shells and tasks; it requires typing update.
Newer remote releases reject older clients. Recovery provides a temporary
SSH shell; reconnect after maintenance to restore sshore features.
` + sshOptions + "Shell connections also accept native OpenSSH -L, -R and -D.\n"

var commandHelp = map[string]string{
	"launch": `Usage: sshore launch [HOST [NAME]] [options] [-- SSH_OPTIONS]
       sshore launch --local [NAME] [options]

Start a new shell. It survives detach and connection loss; exit ends it.
Omit HOST for local use and NAME for an automatic name. To resume, use attach.

Options:
` + shellStartupOptions + connectionOptions + shellConnectionHelp,
	"spawn": `Usage: sshore spawn [HOST [NAME]] [options] [-- SSH_OPTIONS]
       sshore spawn --local [NAME] [options]

Start a temporary shell bound to the launching parent. It ends when that
parent exits; a remote spawn also ends if its lifetime SSH connection is lost.
Detach and attach do not change the owner. Use launch for a persistent shell.

Options:
` + shellStartupOptions + connectionOptions + shellConnectionHelp,
	"attach": `Usage: sshore attach [HOST [NAME]] [options] [-- SSH_OPTIONS]
       sshore attach ID
       sshore attach --local [NAME]

Resume a detached shell. This never creates a shell or evicts its current client.
Omit HOST and NAME to reuse the last successful connection. HOST alone reuses
its remembered shell or offers a selection if there is no history.
Use an ID from list, or --local to select a local shell.

Options:
` + connectionOptions + shellConnectionHelp,
	"detach": `Usage: sshore detach

Disconnect this client and cancel its transfers. A persistent shell keeps
running; a spawn still follows its owner and connection lifetime.
Use attach to reconnect. Ctrl+B d also detaches.
`,
	"rename": `Usage: sshore rename HOST OLD NEW [--server PATH] [-- SSH_OPTIONS]
       sshore rename ID NEW
       sshore rename --local OLD NEW

Rename a shell without restarting its tasks. NEW must be unused.
Pending approvals for the old name must be requested again.
Remote rename requires matching releases and never installs software.
` + sshOptions,
	"list": `Usage: sshore list [HOST] [options] [-- SSH_OPTIONS]

Without HOST, show local tasks and saved remote observations without SSH.
With HOST, query that account live; the remote release must match.
CHECKED shows when state was last confirmed, not whether it is still live.
Peer entries are read-only. Use IDs from this list with attach, rename,
cancel or review; a stale ID never selects a replacement task.

Options:
  --local          Show only local shells
  --verbose        Show lifetime, detailed state and recent shell history
  --json           Emit structured results, including recent shell history
  --server PATH    Select the remote executable

Examples:
  sshore list
  sshore list work --verbose
` + sshOptions,
	"pstree": `Usage: sshore pstree [HOST | --local] [--server PATH] [-- SSH_OPTIONS]

Show shells grouped by host. Branches show original spawn ownership;
attach via shows the attachment source. These are recorded relationships,
not a live OS process tree. @HOST marks a host change within a branch.
CHECKED: now is a live result; never means no recorded check.
Give HOST to query that remote account; omit it for local and saved records.
` + sshOptions,
	"cancel": `Usage: sshore cancel ID
       sshore cancel --local NAME
       sshore cancel {review|mount|forward}/NAME
       sshore cancel HOST NAME [--server PATH] [-- SSH_OPTIONS]
       sshore cancel [HOST] --all [--server PATH] [-- SSH_OPTIONS]

Cancel by ID from list, or by name:
  Shell     Ends the shell and its identified tasks without confirmation.
            Unsaved work may be lost. Confirmed ended history is only cleared.
  Review    Discards the pending request without executing it.
  Mount     Unmounts first; a busy mount stays active for retry.
  Forward   Closes the forward and its active connections.

--all previews the affected tasks and requires typing cancel. Without HOST it
covers local shells and resources. With HOST it targets that account and
local resources connected to HOST. Peer records are read-only.
Remote cancellation requires matching releases and never installs software.
` + sshOptions,
	"review": `Usage: sshore review [NAME | ID] [--stay]

Run locally after a remote request detaches. Omit NAME to select a request,
or use a name or task ID from list. Selecting it opens review only.

Submitting all valid paths or the port starts the operation without another
confirmation. Ctrl-C cancels before submission. A request is single-use,
even if execution fails, and expires after 10 minutes.

File copies resume the source shell when done; --stay returns locally.
Mounts and forwards run independently and leave the source detached.
To discard a request: sshore cancel review/NAME.
`,
	"cp": `Usage: sshore cp SOURCE... TARGET [-- SSH_OPTIONS]

Use HOST:PATH for one side and a local path for the other. Bracket IPv6
addresses; use ./ for local filenames containing a colon. Copies up to
16 regular files; no recursion, symlinks or overwrites. Multiple sources
need a target directory. File destinations must support hard links.

  sshore cp report.zip work:/tmp/
  sshore cp work:/tmp/report.txt -      Write one file to stdout
  sshore cp - work:/tmp/report.txt      Read stdin into a new remote file

Use origin: (or :) to pick a local path. From a remote shell, it refers to
the attached client: sshore cp report.zip origin: queues a request and
detaches. Run sshore review locally. An origin:PATH is only a suggestion.
Streams are for direct copies only.

A failure stops the batch and keeps completed files. Stdout may already
contain partial data. Updating older remote software requires typing update
and can end the listed shells and tasks.
` + sshOptions,
	"mount": `Usage: sshore mount HOST [DIRECTORY [MOUNTPOINT]] [options] [-- SSH_OPTIONS]
       sshore mount DIRECTORY origin:[MOUNTPOINT] [--read-only]

Mount a remote directory using SFTP. Defaults: remote home; ~/mnt/NAME
on Linux or a free drive on Windows. Requires FUSE or WinFsp respectively.
A custom mountpoint must be an existing empty directory on Linux, or a
free drive letter or nonexistent directory on Windows.

Options:
  --name NAME                  Name used by list and cancel mount/NAME
  --read-only                  Reject changes through the mount
  --foreground                 Run in this terminal; Ctrl-C requests unmount
  --no-watch                   Plain SFTP without the remote sshore helper
  --server PATH                Remote helper path; incompatible with --no-watch
  --io-timeout DURATION        Per SFTP request (60s; 0s disables)
  --startup-timeout DURATION   Startup including authentication (0s disables)
  --stats                      Report request and cache statistics
  --direct-io                  Disable kernel read caching (Linux)

From a remote shell, origin: queues a request and detaches. Run review
locally to choose a mountpoint and start the mount.
Busy mounts stay active. A second Ctrl-C during unmount forces disconnect
and can interrupt writes. Request timeouts close the whole mount connection;
affected writes are not retried.
Updating older remote software requires typing update and can end listed tasks.

Example: sshore mount work /srv/project --read-only --name project
` + sshOptions,
	"forward": `Usage: sshore forward LISTENER SERVICE [options] [-- SSH_OPTIONS]

Connections enter LISTENER and go to SERVICE. Use HOST:PORT for the remote
side and a bare port or origin:PORT for the local side. Both use loopback.
Use origin: without a port to choose the local port interactively.

  sshore forward 8080 work:3000        Local listener -> remote service
  sshore forward work:3000 8080        Remote listener -> local service

From a remote shell, use a bare server port and origin:[PORT] for the
client side. The request detaches; run review locally to set the client port.

Options:
  --name NAME    Name used by list and cancel forward/NAME
  --foreground   Run in this terminal; Ctrl-C stops forwarding
  --retries N    Retry lost background SSH connections (0–10; default 0)

Retries cannot prompt for credentials or host keys. Existing connections
do not survive reconnect. Native OpenSSH -L/-R/-D are not accepted here.
` + sshOptions,
	"reset": `Usage: sshore reset

Replace this account's local identity and clear connection history, shell
snapshots and remembered identity routes. Active tasks, approvals and
task IDs stay intact. Active connections may repopulate snapshots.
To resolve a cloned identity, run this on the clone.
`,
	"completion": `Usage: sshore completion SHELL

Load completion in your shell, or add the matching line to its startup file:
  Bash:       source <(sshore completion bash)
  Zsh:        source <(sshore completion zsh)   # after compinit
  Fish:       sshore completion fish | source
  PowerShell: sshore completion powershell | Out-String | Invoke-Expression

Completion reads local hints only and never connects to a remote host.
`,
}

func showHelp(args []string, w io.Writer) error {
	if len(args) == 0 {
		aliases := make([]string, 0, len(commandAliases))
		for alias, command := range commandAliases {
			aliases = append(aliases, alias+"="+command)
		}
		sort.Strings(aliases)
		_, err := io.WriteString(w, help+"\nAliases: "+strings.Join(aliases, ", ")+"\n")
		return err
	}
	if len(args) != 1 {
		return usage("usage: sshore help [COMMAND]")
	}
	topic := canonicalCommand(args[0])
	h, ok := commandHelp[topic]
	if !ok {
		return usage("unknown help topic: %s", args[0])
	}
	_, err := io.WriteString(w, h)
	return err
}
