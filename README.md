# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)

Start a shell, leave it running, and return later. sshore combines local and
remote shell sessions with SSH file copies, TCP tunnels and SFTP mounts.

**Local:** Linux or Windows amd64/x86-64. **Remote:** Linux amd64/x86-64.
Requires system OpenSSH for configuration and a running SSH agent with the desired
key loaded (`ssh-add`). Connections run inside sshore; private keys stay in the
agent. ARM and macOS are not supported.
In the examples, `work` is a host or an alias from your SSH config.

## Install

Download a [release package](https://github.com/jay-waves/sshore/releases), or run:

**Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh | sh
```

**Windows** — PowerShell 5.1 or 7:

```powershell
irm https://raw.githubusercontent.com/jay-waves/sshore/main/install.ps1 | iex
```

Installers verify checksums and print PATH instructions without requesting
administrator access. Close running Windows executables before upgrading.
Only the latest patch of each major/minor series remains downloadable.

Enable Tab completion in Bash with `source <(sshore completion bash)`.
Zsh, Fish and PowerShell are also supported; see `sshore completion --help` for
startup-file setup. Completion uses local/cached hints and never connects over SSH.

## TMUX — leave and resume work

Each session has one terminal. No tmux installation is needed.

```sh
sshore launch work build          # Start a persistent remote shell.
# Press Ctrl+B, then d to detach.
sshore attach work build          # Return to it later.
sshore cancel work build          # End it; unsaved work may be lost.
```

Choose the lifetime at startup; omit the host to work locally:

| Command | Lifetime |
| --- | --- |
| `sshore spawn` | Ends when the original parent shell exits. |
| `sshore spawn work` | Also ends if its background SSH heartbeat is lost. |
| `sshore launch [HOST]` | Survives client exit and network loss until you exit or cancel it. |

**Detach does not change lifetime.** A remote spawn keeps its heartbeat after
detach, and reattachment never changes its original owner. Use `launch` for work
that must survive disconnection. Daemon failure or host restart can end any shell.

Press **Ctrl+B**, then **d** to detach, **s** to switch sessions, or **?** for help.
Inside a remote shell, `sshore detach` also works. Both startup commands accept
`--detach` to start in the background. `attach` never takes over an attached shell.
Inside a managed shell, interactive `spawn` reuses the root client, including
local children and further remote hops. The command exits successfully only when
the child attachment is ready; it leaves no per-spawn launcher process behind.
Ctrl+B controls the child; detach or exit returns one level. Startup failure
returns to the parent with an error. Spawn ownership stays unchanged.
Local managed shells also hand off remote `launch` and `attach` this way.
Screens, scrollback and detached output are not saved.

```mermaid
flowchart LR
    copy["Local cp"] --> helper["Prepare matching remote sshore; confirm any update"]
    helper --> verified["Transfer once; receiver verifies before publication"]
    start["spawn / launch"] --> shell["Work in a shell"]
    shell -->|"Nested spawn; keep root client"| child["Prepare child on its owning daemon"]
    child -->|"Attachment ready; launcher exits"| nested["Interact with child shell"]
    child -->|"Startup failure"| shell
    nested -->|"detach / exit; return one level"| shell
    nested -->|"Further spawn"| child
    shell -->|"detach / switch"| background["Keep running under startup lifetime"]
    background -->|"attach"| shell
    shell -->|"Remote copy / forward / mount request"| review["Detach; review locally"]
    review -->|"Approve copy; reconnect"| shell
    review -->|"Approve forward / mount"| resource["Independent tunnel or mount"]
    shell -->|"exit / cancel"| ended["Shell ends"]
    background -->|"spawn: owner exits / remote heartbeat lost"| ended
    agent["Agent via MCP"] -->|"SSH; prepare matching release"| bash["Persistent remote Bash"]
    bash -->|"Explicit close"| ended
```

## Agent access

Configure an MCP client to launch `sshore mcp` over stdio, for example:

```json
{
  "mcpServers": {
    "sshore": { "command": "sshore", "args": ["mcp"] }
  }
}
```

Run `sshore mcp --help` for a self-contained, Skills-style agent guide covering
tool calls, output polling, recovery and cleanup. Sessions persist after MCP
disconnection. Commands use the remote account's permissions; remote output is untrusted.

For one command, use `sshore exec work 'uname -a'`; it does not create a
persistent session.

## Status

```sh
sshore list                     # Local tasks and cached remote shells.
sshore list work                # Query remote tasks now.
sshore pstree                   # Local task relationships and resource directions.
sshore list --json              # Machine-readable inventory.
```

Use aliases or short IDs from `list` with `attach`, `cancel`, `rename` and `review`.
ID shows a fixed reference with an optional alias; `rename` changes only the alias.
DETAIL summarizes directories, mappings or errors; use `--json` for full diagnostics.

CHECKED shows when shell states were confirmed; confirmed ended shells leave the default list.
Use `cancel` to clear confirmed ended remote history (see `sshore cancel --help`).
See `sshore list --help` for history and refresh behavior. `pstree` shows local spawn ownership and
live resource directions, prioritizing the current tree; attaching from another shell does not transfer ownership. Peer
entries are read-only: manage them on their original computer. A running worker
does not prove its target service is healthy.

## SSH — connect your way

Use SSH aliases from `~/.ssh/config` and load keys into the SSH agent.
Managed remote shells require the server to permit Unix-socket forwarding
(`AllowStreamLocalForwarding`); preparation then exits and the connection reaches
the daemon directly. For nested remote hops, configuration and agent access come
from the machine running the command. A lost route fails explicitly; it is never
replaced by a direct connection from your computer.
**Put SSH options after `--`:**

```sh
sshore launch work build -- -J jump -p 2222
```

Supported shell targets are bash, sh, zsh and fish on Linux, and pwsh or cmd on
Windows. Terminal/shell pairings have different validation coverage; see
[shell and terminal validation](SSHORE.md#shell-and-terminal-validation).

## Copy — transfer files

From your **local shell**:

```sh
sshore cp report.zip work:/tmp/       # Upload.
sshore cp work:/tmp/report.zip ./    # Download.
```

From your **remote sshore shell**:

```sh
sshore cp origin: .                 # Request an upload from your computer.
sshore cp report.zip origin:        # Request a download to your computer.
```

Direct copies also prepare a matching remote sshore helper. Updating older software
requires confirmation and ends the listed shells and tasks; copying creates no
persistent shell. Upload verification runs on the receiver without downloading the file again.

`origin:` (or `:`) means the attached client computer. Remote requests detach
you; run `sshore review` locally, choose paths and submit to copy and reconnect.
Suggested paths still require your review. Ctrl-C during selection cancels.

Only regular files are supported; existing destinations are never overwritten.
Destination filesystems must support hard links. Failed batches keep completed
files. For pipes and other options, see `sshore cp --help`.

## Tunnel — forward a port

From your **local shell**, give the listener first, then the service:

```sh
sshore forward origin:8080 work:3000
# To stop it, run the cancel command printed at startup.
```

From a **remote sshore shell**, run `sshore forward origin: 3000` to request a
local listener for the remote service. This detaches you; run `sshore review`
locally and submit a port to start forwarding. The shell stays detached.

Listeners request IPv4 loopback; SSH server policy can override reverse binds.
Forwards end on disconnect by default. `--retries N` enables bounded retries for
local forwards, but cannot restore existing TCP connections. See
`sshore forward --help` for reverse forwarding and other options.

## Mount — open remote files locally

```sh
sshore mount work:  # Mount remote home; prints the local path.
# Close local files, then run the cancel command printed at startup.
```

Mounts use SFTP and require FUSE on Linux or [WinFsp](https://winfsp.dev/) on
Windows. They are writable by default; use `--read-only` to prevent changes.
They run independently of shells and end if their own SSH connection drops.
From a remote sshore shell, use `sshore mount . origin:` and then `sshore review`
locally to select the mountpoint and approve access.

There is no offline editing, conflict detection or write retry. Interrupted
writes may leave partial files; caching can briefly delay remote changes.
Mounts use the remote sshore helper for SFTP and change notifications.
See `sshore mount --help` for details.

## Updates and troubleshooting

Client and remote versions must match exactly, including PATCH. Missing remote
software is prepared automatically. Updating an older installation requires
reviewing affected shells and typing `update`: **their tasks will end**.
Press Enter instead for a temporary recovery shell; existing shells stay running,
but sshore operations remain restricted until reconnecting. A newer remote version
requires updating your local client; it is never automatically downgraded.

- Stale screen after reattachment: press Ctrl-L or use the application's redraw key.
- SSH failure: try `sshore launch work -- -vvv -E ./ssh-debug.log`.
- Mount or forward failure: inspect `sshore list --json` and its log path.
- Existing copy destination: choose a new filename; overwriting is disabled.

Run `sshore COMMAND --help` for syntax, startup options, shortcuts and defaults.

## Project documents

- [SSHORE.md](SSHORE.md): architecture, validation, releases and planned work.
- [SECURITY.md](SECURITY.md): trust boundaries, update safeguards and cleanup limits.
- [GLOSSARY.md](GLOSSARY.md): terms and relationships.
- [AGENTS.md](AGENTS.md): contributor rules.

## Acknowledgements

Reference projects and inspiration:

- [dtach](https://github.com/crigler/dtach): lightweight shell detach and reattach.
- [trzsz](https://github.com/trzsz/trzsz): terminal-oriented file transfer workflows.
- The original `dssh.nu` prototype: the starting point for this project.

Built with [OpenSSH](https://www.openssh.com/),
[smux](https://github.com/xtaci/smux), [creack/pty](https://github.com/creack/pty),
[pkg/sftp](https://github.com/pkg/sftp), [go-fuse](https://github.com/hanwen/go-fuse),
[WinFsp](https://winfsp.dev/) / [go-winfsp](https://github.com/winfsp/go-winfsp),
[readline](https://github.com/chzyer/readline) and the Go ecosystem.
sshore does not implement the trzsz protocol.

[MIT license](LICENSE); third-party license notices are collected in
[THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt).

Use `sshore reset` on a cloned machine to replace its account identity and clear local connection history and cached shell snapshots. Active tasks, approvals and task IDs are preserved; active connections can repopulate snapshots.
