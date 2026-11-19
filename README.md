# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)

Start a shell on this computer or over SSH, detach, and pick up where you left
off. You can also copy files, forward ports and mount remote directories.

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

Linux and Windows clients require system OpenSSH. Remote shell hosting requires
Linux. See [OS and shell support](#os-and-shell-support).

Run `sshore install` to update this computer to the latest release. Run
`sshore install work:` to install this client's exact version on a remote Linux
host named `work`. Use `work:/absolute/path/to/sshore` for a custom install path.
Client and remote versions must match, including the patch version. When
upgrading from v0.7.9, use the standalone installer above on each target.

When replacement is needed, the installer stops the target account’s daemon.
If it owns tasks, review the list and type `kill` to end them and continue.
For remote installs, enter the confirmation in your local terminal. On Windows, close other
clients using the executable and reopen your terminal if PATH changed.

## Reading `HOST:VALUE`

The part before the colon tells you **which machine**. The command tells you
**what the value on that machine means**. For example, `work:7afe22` selects a
shell for `attach`, `work:/tmp/report.zip` names a file for `cp`, and
`work:3000` names a port for `forward`. Here, `work` can be a hostname or an
SSH config alias.

Keeping the machine alongside each value makes direction visible in the command.
For example, `sshore cp .:report.zip work:/tmp/` reads the file on the machine
running the command and writes it on `work`. This also works from inside an
attached remote shell, where “here” and “my computer” can mean different things.

| Endpoint | Where it points |
| --- | --- |
| `work:` | The named host. An empty value takes the command's default, such as the remote home directory for `mount`. |
| `self:` or `.:` | The machine running the command. For example, `.:report.zip` is relative to the current directory there. |
| `origin:` or `::` | The attached client, when requesting a copy, forward or mount from a remote shell. You approve the request on that client. |

Use an empty value for host-only commands such as `sshore list work:` and
`sshore exec work: 'uname -a'`. Both still accept the plain SSH host for
compatibility.

## Start and resume shells

```sh
sshore launch work:         # Start a persistent shell. Omit work: for a local one.
# Press Ctrl+B, then d to detach. Return later with:
sshore attach               # Resume the most recently detached local or remote shell.
sshore attach work:         # Resume the most recently detached shell on work.
sshore list work:           # Find a specific shell's ID or name.
sshore attach work:7afe22   # Resume that shell.
```

Choose `launch` for work that should survive a client exit or network loss.
Choose `spawn` (or its alias `sh`, as in `sshore sh work:`) for a shell that
should end when its original parent exits.
A remote spawn also ends if its SSH heartbeat times out. Detaching and reattaching
preserve the original lifetime. Both commands accept `--detach` to start in the
background. Daemon failure or a host restart can end any shell. Screens and
scrollback are not saved.

Each session has one terminal and requires no tmux installation.
While attached, sshore requests a terminal window title containing the session
endpoint. Applications may change it, and supported terminals restore the prior
title when the attachment ends.

Press **Ctrl+B** and then one of these keys:

| Key | Action |
| --- | --- |
| `d` | Detach. Run `sshore attach` to return to the last detached local or remote shell. |
| `l`, `n`, `p` | Switch to the last, next or previous session. |
| `c` | Spawn a nested shell on the current host. Exit or detach to return one level. |
| `r` | Request a redraw. |
| `:` | Open the full-screen sshore command prompt. |
| `Ctrl+B` | Send a literal Ctrl+B to the application. |

At the command prompt, use `list` for task inventory, `ps` for the local task
tree, `attach <ID|name>` to switch to a detached session in the current scope,
`spawn` to open a nested shell, and `launch` to open an independent shell on
the current host. Tab completes commands, session names and
short IDs. Use the arrow keys to edit input or recall commands from the current
prompt. Type `help` for commands. Press Esc
or type `exit` to return. Applications already using the alternate screen
redraw on return.

Use `cp self:FILE HOST:PATH` or `forward self:PORT HOST:PORT` to run the
corresponding local CLI command after detaching. Both endpoints are required.
Quote paths with spaces. Tab completes command names, flags, and local
`self:` paths for `cp`. Enter remote paths and ports manually.

Use `--no-escape` if the application needs all keystrokes.

## Find and manage tasks

```sh
sshore list                  # Local tasks and cached remote shells.
sshore list work:            # Query the remote host now.
sshore pstree                # Show local task relationships.
sshore list --json           # Include diagnostics and resource log paths.
```

When a native local shell hands its terminal to another sshore session, `list`
adds `-> HOST` (or `-> local`) to DETAIL, and `pstree` shows the same destination.
The source shell can be attached while it is detached. If the original frontend
cannot return to it, it prints the reason and exits.

Use an ID or name from `list` to select a task. For example,
`sshore rename work:7afe22 build` lets you resume it with
`sshore attach work:build`. To end the shell, run `sshore kill work:build`.
A bare selector such as `sshore kill 123eef` searches only locally managed
tasks, like `sshore kill self:123eef`. For a remote row from cached or live
output, include its host, as in `sshore kill work:123eef`. Killing a shell can
discard unsaved work. Manage read-only peer entries on their original
computer.

## Connect over SSH

Use hosts and aliases from your SSH config. For public-key login, load the key
with `ssh-add`. Password and keyboard-interactive login require a terminal.
Managed remote shells require the SSH server to allow TCP forwarding to
loopback. Put SSH options after `--`, for example:

```sh
sshore launch work: -- -J jump -p 2222
```

For a single command, run `sshore exec work: 'uname -a'`. For port forwarding,
use `sshore forward`.

## OS and shell support

| OS (amd64/x86-64 only) | bash | sh | zsh | fish | PowerShell 7 (pwsh) | cmd |
| --- | --- | --- | --- | --- | --- | --- |
| Linux | ✓ | ✓ | ✓ | ✓ | — | — |
| Windows 11 / Server 2025 or newer | — | — | — | — | ✓ | ✓ |

Windows can connect to Linux hosts and run local shells.

## Copy files

```sh
sshore cp .:report.zip work:/tmp/     # Upload to the remote directory.
sshore cp work:/tmp/report.zip .:./   # Download to the current directory.
```

Direct copies require a matching remote sshore helper. Copy supports regular
files and refuses to overwrite existing destinations. If a batch fails, files
already copied stay in place.

In copy and mount endpoints, `~` and `~/...` refer to the home directory on
that endpoint's machine. For example, `sshore cp .:report.zip work:~` saves
`report.zip` in the remote user's home directory. Use `./~` for a path named
`~`.

## Forward a port

```sh
sshore forward .:8080 work:3000
```

Connect to port 8080 on this computer to reach port 3000 on `work`. The listener
comes first and requests an IPv4 loopback address. For reverse forwards, SSH
server policy can override the bind address. A disconnect ends the forward by
default. To stop it yourself, run the kill command printed at startup.

## Mount remote files

```sh
sshore mount work: .: --read-only
```

This mounts the remote home directory and prints the local mountpoint. Give a
path after either colon to choose a directory. Mounts use SFTP and require FUSE
on Linux or [WinFsp](https://winfsp.dev/) on Windows. Omit `--read-only` to
allow writes. An interrupted write can leave a partial file. Close open files
before running the kill command printed at startup.

An `origin:~/...` path expands on the client during review.

## Request access from a remote shell

Inside an attached remote shell, use `origin:` when an operation needs a file,
port or mountpoint on your client computer.

```sh
sshore cp .:report.zip origin:    # Send a remote file to your computer.
sshore cp origin: .:./            # Ask for a file from your computer.
sshore forward origin: .:3000     # Request a client port for this remote service.
sshore mount .:./ origin:         # Request a client mountpoint for this directory.
```

Return to the client and run `sshore review`. The input field starts with the
suggested path or port when supplied, or a local default when available. Edit it
if needed and press Enter to approve. Without a suggested path, downloads default
to the client directory recorded when the request was queued. For batch downloads, submit each destination
before the transfer starts. A forward request detaches the shell. Use `attach` to
resume it after review.

## Help and troubleshooting

- For common examples, run `sshore --tldr` or `sshore cp --tldr`. For options
  and defaults, run `sshore COMMAND --help`.
- If a reattached screen looks stale, press Ctrl-L or use the application's redraw key.
- To diagnose SSH failures, run `sshore launch work: -- -vvv -E ./ssh-debug.log`.
- For a failed mount or forward, check the log path in `sshore list --json`.
- To reset this computer, save your work and run `sshore reset` outside managed
  shells. It ends local tasks and clears local application state and caches.

## Project documents

- [SSHORE.md](SSHORE.md): architecture, validation, releases and planned work.
- [SECURITY.md](SECURITY.md): trust boundaries, update safeguards and cleanup limits.
- [GLOSSARY.md](GLOSSARY.md): terms and relationships.
- [AGENTS.md](AGENTS.md): contributor rules.

## Acknowledgements

Reference projects and inspiration:

- [dtach](https://github.com/crigler/dtach): lightweight shell detach and reattach.
- [trzsz](https://github.com/trzsz/trzsz): terminal-oriented file transfer workflows.
    sshore does not implement the trzsz protocol.

Built with

- [OpenSSH](https://www.openssh.com/),
- [smux](https://github.com/xtaci/smux),
- [creack/pty](https://github.com/creack/pty),
- [pkg/sftp](https://github.com/pkg/sftp),
- [go-fuse](https://github.com/hanwen/go-fuse),
- [WinFsp](https://winfsp.dev/) / [go-winfsp](https://github.com/winfsp/go-winfsp),
- [readline](https://github.com/chzyer/readline) and the Go ecosystem.

[MIT license](LICENSE); third-party license notices are collected in
[THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt).
