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
  shell       Start a shell (alias: sh)
  attach      Resume a detached shell
  detach      Pin and leave the current shell
  rename      Rename a running shell

Files and ports
  ping        Measure SSH connection and echo latency
  cp          Copy files over SSH
  mount       Mount a remote directory
  forward     Forward a TCP port

Tasks and installation
  copy-id     Install an SSH public key on a host
  pstree      Show the shell tree (ps)
  list        Show task details (ls)
  kill        Kill shells or stop other tasks
  bootstrap   Reset the target account and reinstall sshore
  reset       End local tasks and clear application state
  daemon      Run the account task manager

Start: sshore shell work:  Resume: sshore attach work:7afe22
In a session: Ctrl+B c opens a nested shell; d pins and detaches; r redraws; s or : opens commands.
Examples: sshore --tldr             Details: sshore COMMAND --help
Version: sshore --version
`

const sshOptions = `
--port PORT selects the SSH port (1–65535). Other SSH options follow --,
for example: --port 2222 -- -J jump. The legacy -- -p PORT form is supported.
Do not combine --port with SSH -p or -o Port.
Configuration options: -p -i -J -F -l -o -c -m -v/-vv/-vvv -4 -6.
IdentityFile/-i selects private key files. An interactive
terminal is required for passphrases, passwords and keyboard-interactive login.
Forwarding rules in SSH config and -L, -R, -D produce a warning and are ignored.
Use sshore forward for TCP forwarding. Compression, agent/X11 forwarding and
ProxyCommand are unsupported.
`

const connectionOptions = `  --server PATH         Remote executable (default: PATH, then ~/.local/bin/sshore)
  --no-escape           Disable Ctrl+B shortcuts
`

const shellStartupOptions = `  --pin                 Keep the shell until it exits
  --no-login            Use non-login startup (Linux and macOS)
  --no-profile          Skip optional shell startup files; implies --no-login on Linux and macOS
`

const shellConnectionHelp = `
The remote release must match exactly. Update the older side before connecting.
` + sshOptions

const connectionFlags = "--server --no-escape"

var cliCommands = map[string]cliSpec{
	"": {
		flags: []string{"--version"},
		examples: `Start a temporary remote shell
  sshore shell work:
Resume it after detaching with Ctrl+B d
  sshore attach work:7afe22
Upload a file
  sshore cp ./report.txt work:/tmp/report.txt
Forward a local port to a remote service
  sshore forward 8080 work:3000

More examples: sshore shell|cp|mount|forward --tldr
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
	"bootstrap": {
		ssh: true, max: 1, flags: []string{"--apply"},
		help: `Usage: sshore bootstrap [HOST[:PATH]] [-- SSH_OPTIONS]

Reset this account and reinstall the latest release, or reset HOST's account and
install the exact local version over SSH. Every run ends the target's sshore
tasks (including pinned shells) and clears its identity, history and caches.
This also applies when the version already matches. If tasks are running,
review the list and confirm before continuing. Run outside the target's managed shells.
Omit the target or use here: to install locally. Use HOST or HOST: to select a remote
default path, or append an absolute path to either endpoint.
` + sshOptions,
		examples: "Reset and reinstall this account\n  sshore bootstrap\nReset and reinstall the account on work\n  sshore bootstrap work\n",
	},
	"exec": {
		ssh: true, min: 2, max: 2,
		help: `Usage: sshore exec HOST 'COMMAND' [-- SSH_OPTIONS]

Execute COMMAND once over SSH without a remote PTY. Uses SSH client
configuration and private key files. Interactive login requires a terminal.
Standard streams and the SSH exit status pass through.
No persistent sshore shell is created and no remote installation is required.
Use an SSH config host alias or pass connection options after --.

Example: sshore exec work 'uname -a'
` + sshOptions,
	},
	"ping": {
		ssh: true, min: 1, max: 1, flags: []string{"--count"},
		help: `Usage: sshore ping HOST [--count N] [--port PORT] [-- SSH_OPTIONS]

Measure authenticated SSH connection time and character round-trip latency
over one SSH session. The remote account must be able to run cat.
--count accepts 1 to 1000 total samples (default 30). Each group has up to
10 samples; groups are 1 second apart. Echo testing has a 30s budget, excluding
the intervals.
` + sshOptions,
		examples: "Measure latency to an SSH host\n  sshore ping work --count 20\n",
	},
	"shell": {
		ssh: true, max: 1, flags: strings.Fields(connectionFlags + " --pin --no-login --no-profile"),
		help: `Usage: sshore shell [HOST|HOST:NAME|:NAME] [options] [-- SSH_OPTIONS]

Alias: sh

Start a shell on this machine or an SSH host. A shell ends with its starting
connection or parent shell unless started with --pin or explicitly detached.
Opening a shell from an interactive session creates a child. Its parent waits for it to return.
` + shellStartupOptions + connectionOptions + shellConnectionHelp,
		examples: "Open a remote shell\n  sshore shell work:build\nOpen a local shell\n  sshore shell :build\n",
	},
	"attach": {
		ssh: true, max: 1, flags: strings.Fields(connectionFlags),
		help: `Usage: sshore attach [ID|HOST|HOST:ID|:ID] [options] [-- SSH_OPTIONS]

From a terminal, enter an idle root. Inside a shell, enter a detached child
or adopt an idle root on this daemon. Use HOST:ID for a child opened over SSH.
With no target, resume the most recently detached eligible local shell.
A bare hexadecimal ID or unique prefix selects a local shell or one with a
saved direct SSH connection. Use HOST:ID to specify the
remote host, or HOST: to resume there (including hexadecimal hosts such as db:).
Use here: for the local machine, or .: as shorthand. IDs accept unique
hexadecimal prefixes.

Options:
` + connectionOptions + shellConnectionHelp,
	},
	"detach": {
		max: 1,
		help: `Usage: sshore detach [DURATION]

Pin the current shell and return to its parent or the root prompt.
Detach keeps its parent edge. Omit DURATION to keep the shell until it exits.
Use attach to resume it. Ctrl+B d also pins and detaches.
`,
	},
	"rename": {
		ssh: true, min: 2, max: 2, flags: []string{"--server"},
		help: `Usage: sshore rename HOST:ID NEW [--server PATH] [-- SSH_OPTIONS]
       sshore rename ID NEW
       sshore rename {mount|forward}/OLD NEW

Set or replace a shell or resource alias without restarting it.
NEW must be unused and cannot be 6–32 hexadecimal characters.
A short ID from list may replace OLD; the task ID stays unchanged.
Remote rename requires matching releases and never installs software.
` + sshOptions,
	},
	"pstree": {
		help: `Usage: sshore pstree  (alias: ps)

Show shell names, short IDs and states in their parent-child tree.
A leading * marks the current shell. Use list for details.
`,
	},
	"list": {
		flags: []string{"--json"},
		help: `Usage: sshore list [--json]  (alias: ls)

List shell, mount and forward details, including incoming resources. PARENT
shows the parent shell's short ID. DETAIL includes connection hosts and
port mappings. A forward's arrow points toward the target service.
Use --json for machine-readable task details and exact timestamps.
`,
	},
	"kill": {
		ssh: true, max: 1, flags: []string{"--server", "--all"},
		help: `Usage: sshore kill ID
       sshore kill {mount|forward}/NAME
       sshore kill HOST:ID [--server PATH] [-- SSH_OPTIONS]
       sshore kill [HOST:] --all [--server PATH] [-- SSH_OPTIONS]

Use a name or short ID from list as the endpoint value. Use :ID for a locally managed
task and HOST:ID for a remote shell. IDs stay bound to their original
instance across renames; an unavailable instance cannot select its replacement.

Effects:
  Shell     Kills the shell and its identified processes without confirmation.
            Unsaved work may be lost.
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
Use a bare path for the mountpoint. With an empty here: path, Linux and macOS
use ~/mnt/NAME, with the registered task name for background mounts or a fresh
six-character name for foreground mounts. Windows selects a free drive letter.
Requires FUSE on Linux, WinFsp on Windows, or macFUSE with its kernel extension
on Apple Silicon Macs (experimental).
A custom mountpoint must be an existing empty directory on Linux or macOS, or a
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

Requires FUSE on Linux, WinFsp on Windows, or macFUSE with its kernel extension
on Apple Silicon Macs (experimental).
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
