package main

import (
	"github.com/jay-waves/sshore/internal/termui"
	"io"
	"os"
	"sort"
	"strings"
)

const help = `Usage: sshore COMMAND [ARGS]

Shells
  exec        Execute a command over SSH
  launch      Start a persistent shell
  spawn       Start a shell that ends with its owner
  attach      Resume a detached shell
  detach      Leave the current shell running
  rename      Rename a running shell

Files and ports
  ping        Measure SSH connection and echo latency
  cp          Copy files over SSH
  mount       Mount a remote directory
  forward     Forward a TCP port

Tasks and installation
  copy-id     Install an SSH public key on a host
  list        Show tasks and when they were last checked
  pstree      Show shell ownership and attachments
  kill        Kill shells or stop other tasks
  install     Install or update sshore
  reset       End local tasks and clear application state
  daemon      Run the account task manager

Start: sshore launch work:     Resume: sshore attach work:7afe22
In a session: Ctrl+B c spawns; d detaches; n/p switches; l returns; r redraws; s or : opens commands.
Examples: sshore --tldr             Details: sshore COMMAND --help
Version: sshore --version
`

const sshOptions = `
--port PORT selects the SSH port (1–65535). Other SSH options follow --,
for example: --port 2222 -- -J jump. The legacy -- -p PORT form is supported.
Do not combine --port with SSH -p or -o Port.
Configuration options: -p -i -J -F -l -o -E -v/-vv/-vvv -4 -6.
IdentityFile/-i selects private key files. An interactive
terminal is required for passphrases, passwords and keyboard-interactive login.
SSH config forwarding is disabled; use forward. Compression, agent/X11
forwarding and ProxyCommand are unsupported; unsupported requests fail.
`

const connectionOptions = `  --server PATH         Remote executable (default: PATH, then ~/.local/bin/sshore)
  -q, --quiet           Hide periodic transfer progress
  --no-escape           Disable Ctrl+B shortcuts
`

const shellStartupOptions = `  --detach              Start without attaching
  --no-login            Use non-login startup (Linux)
  --no-profile          Skip optional shell startup files; implies --no-login on Linux
`

const shellConnectionHelp = `
The remote release must match exactly. Update the older side before connecting.
` + sshOptions + "Shell connections also accept SSH -L, -R and -D.\n"

const connectionFlags = "--server --quiet --no-escape"

var cliCommands = map[string]cliSpec{
	"": {
		flags: []string{"--version"},
		examples: `Start a persistent remote shell
  sshore launch work:
Resume it after detaching with Ctrl+B d
  sshore attach work:7afe22
Upload a file
  sshore cp ./report.txt work:/tmp/report.txt
Forward a local port to a remote service
  sshore forward 8080 work:3000

More examples: sshore launch|spawn|cp|mount|forward --tldr
`,
	},
	"help": {max: 1},
	"copy-id": {
		ssh: true, min: 1, max: 1,
		help: `Usage: sshore copy-id HOST [-- SSH_OPTIONS]

Install a public key from the host's effective IdentityFile settings into
the remote account's ~/.ssh/authorized_keys. If several keys are available,
choose one in the terminal with Tab completion.
` + sshOptions,
		examples: "Install the configured public key\n  sshore copy-id work\n",
	},
	"install": {
		ssh: true, max: 1, flags: []string{"--apply"},
		help: `Usage: sshore install [HOST[:PATH]] [-- SSH_OPTIONS]

Update this computer to the latest published release, or update HOST to the
exact local version over SSH. Before replacement, install stops the target daemon.
If it owns tasks, review the list and follow the confirmation prompt.
Omit the target or use here: to install locally. Use HOST or HOST: to select a remote
default path, or append an absolute path to either endpoint.
` + sshOptions,
		examples: "Update this computer\n  sshore install\nUpdate a remote host to match this computer\n  sshore install work\n",
	},
	"exec": {
		min: 2, max: 2,
		help: `Usage: sshore exec HOST 'COMMAND'

Execute COMMAND once over SSH without a remote PTY. Uses system ssh -G
configuration and private key files. Interactive login requires a terminal.
Standard streams and the SSH exit status pass through.
No persistent sshore shell is created and no remote installation is required.
Use an SSH config host alias for connection options.

Example: sshore exec work 'uname -a'
`,
	},
	"ping": {
		ssh: true, min: 1, max: 1, flags: []string{"--count"},
		help: `Usage: sshore ping HOST [--count N] [--port PORT] [-- SSH_OPTIONS]

Measure authenticated SSH connection time and character round-trip latency
over one SSH session. The remote account must be able to run cat.
--count accepts 1 to 1000 samples (default 10). Echo testing has a 30s budget.
` + sshOptions,
		examples: "Measure latency to an SSH host\n  sshore ping work --count 20\n",
	},
	"launch": {
		ssh: true, native: true, max: 1, flags: strings.Fields(connectionFlags + " --detach --no-login --no-profile"),
		help: `Usage: sshore launch [HOST|HOST:NAME|:NAME] [options] [-- SSH_OPTIONS]

Start a new shell. It survives detach and connection loss; exit ends it.
Omit the target for a local shell or use HOST for a remote shell.
Use :NAME to name a local shell or HOST:NAME to name a remote shell.
To resume, use attach.

Options:
` + shellStartupOptions + connectionOptions + shellConnectionHelp,
		examples: `Start a persistent remote shell
  sshore launch work:
Start a local shell
  sshore launch :build
Connect through a jump host
  sshore launch work: -- -J jump

Ctrl+B d detaches; sshore attach work:7afe22 resumes.
The remote sshore release must match exactly.
`,
	},
	"spawn": {
		ssh: true, native: true, max: 1, flags: strings.Fields(connectionFlags + " --detach --no-login --no-profile"),
		help: `Usage: sshore spawn [HOST|HOST:NAME|:NAME] [options] [-- SSH_OPTIONS]

Start a shell bound to the launching parent. Omit the target for a local shell
or use HOST for a remote shell. Use :NAME to name a local shell or
HOST:NAME to name a remote shell.
It ends when that parent exits; a remote spawn also ends if its lifetime SSH connection is lost.
Detach and attach do not change the owner. Use launch for a persistent shell.

Options:
` + shellStartupOptions + connectionOptions + shellConnectionHelp,
		examples: `Start a remote shell tied to this owner and connection
  sshore spawn work:build
Start without attaching
  sshore spawn work: --detach

The shell ends when its owner or lifetime connection is lost.
The remote sshore release must match exactly.
`,
	},
	"attach": {
		ssh: true, native: true, max: 1, flags: strings.Fields(connectionFlags),
		help: `Usage: sshore attach [HOST|HOST:ID|:ID] [options] [-- SSH_OPTIONS]

With no target, resume the most recently detached local or remote shell.
Use HOST to resume on that host. Use HOST:ID or :ID to select a shell
by its ID or alias from list.
Use here: for the local machine, or .: as shorthand. IDs accept unique
hexadecimal prefixes.

Options:
` + connectionOptions + shellConnectionHelp,
	},
	"detach": {
		help: `Usage: sshore detach

Disconnect this client and cancel its transfers. A persistent shell keeps
running; a spawn still follows its owner and connection lifetime.
Use attach to reconnect. Ctrl+B d also detaches.
`,
	},
	"rename": {
		ssh: true, min: 2, max: 2, flags: []string{"--server"},
		help: `Usage: sshore rename HOST:ID NEW [--server PATH] [-- SSH_OPTIONS]
       sshore rename here:ID NEW
       sshore rename {mount|forward}/OLD NEW

Set or replace a shell or resource alias without restarting it.
NEW must be unused and cannot be 6–32 hexadecimal characters.
A short ID from list may replace OLD; the task ID stays unchanged.
Remote rename requires matching releases and never installs software.
` + sshOptions,
	},
	"list": {
		ssh: true, max: 1, flags: []string{"--server", "--json"},
		help: `Usage: sshore list [HOST] [options] [-- SSH_OPTIONS]

Without an endpoint, show local tasks, shells hosted by this Unix account, and saved
remote observations without SSH.
Use HOST: to query a remote account live. The remote release must match.
Use here: or .: to show only local shells. Plain SSH hosts still work.
CHECKED shows when state was last confirmed, not whether it is still live.
DETAIL summarizes directories, mappings or errors; terminal output is shortened
to fit the available width. Redirected output preserves full details.
ID shows the first six UID characters followed by an optional (alias).
Use a longer prefix or full UID when a reference is ambiguous.
TYPE uses launch, spawn, mount or forward. Peer entries are read-only.
Use aliases or short IDs for attach, rename and kill.
A matching short ID takes precedence over a name.

Options:
  --json           Emit full diagnostics and recent history as JSON
  --server PATH    Select the remote executable

Examples:
  sshore list
  sshore list work:
` + sshOptions,
	},
	"pstree": {
		help: `Usage: sshore pstree

Show local managed tasks and live local resource transports as TYPE/ID (alias).
The tree containing *current comes first; other nodes sort by newest check.
Branches show local spawn ownership. Cached remote shells and stale peer mirrors
are excluded. <- HOST on a shell identifies its current remote attachment source.
Resource arrows run from provider to consumer: importing a mount or remote service
shows <- HOST; exporting a mount or local service shows -> HOST.
? marks an unconfirmed local entry or an unknown legacy direction.
Incoming resources are read-only here. Use list for states and observation times.
No SSH is started by this command.
`,
	},
	"kill": {
		ssh: true, max: 1, flags: []string{"--server", "--all"},
		help: `Usage: sshore kill :ID
       sshore kill {mount|forward}/NAME
       sshore kill HOST:ID [--server PATH] [-- SSH_OPTIONS]
       sshore kill [HOST:] --all [--server PATH] [-- SSH_OPTIONS]

Use a name or short ID from list as the endpoint value. Use :ID for a locally managed
task and HOST:ID for a remote shell. IDs stay bound to their original
instance across renames; an unavailable instance cannot select its replacement.

Effects:
  Shell     Kills the shell and its identified processes without confirmation.
            Unsaved work may be lost. Confirmed ended history is only cleared.
  Mount     Unmounts first; a busy mount stays active for retry.
  Forward   Closes the forward and its active connections.

--all stops the account daemon, even when idle. Without HOST it targets the
local account. With HOST it targets the remote account and also ends local
resources connected to HOST. When tasks are present, it previews them and
prompts [Y/n]. Enter confirms and n cancels. Peer records are read-only.
--all uses authenticated cross-release daemon shutdown. Individual remote task
control requires matching releases. Neither form installs software.
` + sshOptions,
	},
	"cp": {
		ssh: true, min: 2, max: -1,
		help: `Usage: sshore cp SOURCE... TARGET [-- SSH_OPTIONS]

A bare path belongs to the machine running this command. Use HOST:PATH
for an SSH host. Use here: or .: for an explicit local endpoint.
In an attached remote shell, origin: or :: requests local approval.
Leave a local path empty to choose it. Use - for standard input or output.
Copy supports up to 16 regular files. Batches with an explicit target require a directory.

  sshore cp report.zip work:/tmp/
  sshore cp work:/tmp/report.txt -
  sshore cp - work:/tmp/report.txt

A failure stops the batch and keeps completed files. Stdout may already
contain partial data. The remote sshore release must match exactly.
` + sshOptions,
		examples: `Upload a file
  sshore cp ./report.txt work:/tmp/report.txt
Download a file
  sshore cp work:/tmp/report.txt ./report.txt
Use a different SSH port
  sshore cp ./report.txt work:/tmp/report.txt --port 2222
`,
	},
	"mount": {
		ssh: true, min: 2, max: 2, flags: strings.Fields("--foreground --read-only --io-timeout --startup-timeout --stats --direct-io"),
		help: `Usage: sshore mount HOST:SOURCE MOUNTPOINT [options] [-- SSH_OPTIONS]
       sshore mount here:[DIRECTORY] origin:[MOUNTPOINT] [--read-only]

Mount a remote directory through the remote sshore helper using SFTP.
The remote sshore release must match exactly.
Background mounts receive fixed task IDs. HOST: selects the remote home.
Use [HOST]:SOURCE for an IPv6 address.
Use a bare path for the mountpoint. Use here: with an empty path
to use ~/mnt/NAME on Linux or a free drive letter on Windows.
Requires FUSE on Linux or WinFsp on Windows.
A custom mountpoint must be an existing empty directory on Linux, or a
free drive letter or nonexistent directory on Windows.

Options:
  --read-only                  Reject changes through the mount
  --foreground                 Run in this terminal; Ctrl-C requests unmount
  --io-timeout DURATION        Per SFTP request, including handshake (60s; 0s disables)
  --startup-timeout DURATION   Startup including authentication (0s disables)
  --stats                      Report request and cache statistics
  --direct-io                  Disable kernel read caching (Linux)

From a remote shell, origin: and :: select the attached client.
Approve the request locally and choose a mountpoint.
Busy mounts stay active. A second Ctrl-C during unmount forces disconnect
and can interrupt writes. Request timeouts close the whole mount connection;
affected writes are not retried.

Example: sshore mount work:/srv/project .: --read-only
` + sshOptions,
		examples: `Mount a remote directory at the default local mountpoint
  sshore mount work:/srv/project .:
Mount read-only
  sshore mount work:/srv/project .: --read-only
Run in the foreground
  sshore mount work:/srv/project .: --foreground

Requires FUSE on Linux or WinFsp on Windows.
`,
	},
	"forward": {
		ssh: true, min: 2, max: 2, flags: []string{"--retries", "--foreground"},
		help: `Usage: sshore forward LISTENER SERVICE [options] [-- SSH_OPTIONS]

The listener comes first. Use a bare port for the current machine and
HOST:PORT for an SSH host. Leave the client-side port empty to choose it.
From a remote shell, use origin: or :: for the attached client and request local approval.

  sshore forward 8080 work:3000
  sshore forward work:3000 8080

Options:
  --foreground   Run in this terminal; Ctrl-C stops forwarding
  --retries N    Retry lost background SSH connections (0–10; default 0)

Retries cannot prompt for credentials or host keys. Existing connections
do not survive reconnect. SSH -L/-R/-D are not accepted here.
` + sshOptions,
		examples: `Expose a remote service on a local port
  sshore forward 8080 work:3000
Expose a local service on a remote port
  sshore forward work:8080 .:3000
Run until Ctrl-C
  sshore forward 8080 work:3000 --foreground
`,
	},
	"daemon": {
		max: 1,
		help: `Usage: sshore daemon [mount]

Run this account's task manager in the foreground. Clients start it automatically
when needed. One manager owns shells and resource coordination.
It exits after 30 seconds without tasks or connections.
The mount subcommand runs an isolated mount worker, started by the manager.
`,
	},
	"reset": {
		help: `Usage: sshore reset

Immediately end this account's tasks and clear local identity, history, requests,
logs and cached releases. Unsaved work may be lost. Run outside managed shells.
The program and user files remain. Busy mounts can prevent reset.

`,
	},
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
