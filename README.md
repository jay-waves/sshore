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

Run `sshore bootstrap work` to reset your account on the Linux host `work` and
install the exact local release. Omit `work` to reset this computer's sshore
account and install the latest release. Every run, including a same-version
reinstall, ends the target's tasks (including pinned shells) and clears its
sshore identity, history and cached releases. Review and confirm the list of
running tasks before continuing. Managed connections require matching releases.

## OS and shell support

| Platform (amd64) | Local shells | Remote hosting |
| --- | --- | --- |
| Linux | bash, sh, zsh, fish | Supported |
| Windows 11 / Server 2025 or newer | PowerShell 7, cmd | — |

Both clients can connect to Linux over SSH. sshore reads client configuration
with its built-in resolver. For managed shells, the host's SSH server must allow
TCP forwarding to loopback. See [development notes](SSHORE.md#terminal-and-mux-design-scope)
for the experimental Apple Silicon client, which supports local shells using
the login shell selected by `$SHELL`.

## ssh — Run remote commands and forward ports

The resolver reads the user and system `ssh_config` files. It supports host
aliases, `Include`, static `Match` rules including `Match final`, private
keys, certificates, known-hosts files, jump hosts and algorithm lists. Use
`-- -F FILE` to select a different file, or `-- -F none` for built-in defaults.
Dynamic rules such as `Match exec` and DNS-based hostname canonicalization
produce a configuration error. Any session-routing environment variables
needed by sshore are set automatically.

During SSH login, sshore skips unsupported methods such as GSSAPI and host-based
authentication. It tries the remaining methods offered by the server in
`PreferredAuthentications` order, respecting the local authentication settings.
Public-key authentication uses identity files and their certificates. SSH agent
and PKCS#11 signers are outside the current client scope.
`ConnectTimeout` defaults to 15 seconds. Set it to `none` or `0` to disable the
connection timeout for that host.
The default `ServerAliveInterval` is 15 seconds. Set it to `0` to disable SSH
keepalive probes.

SSH forwarding rules in matching config sections and on the command line
produce a warning and open no listeners. Use `sshore forward` for TCP forwarding.

```sh
sshore exec work 'uname -a'         # Run one command on work.
sshore sh work: -- -J jump       # Open a remote shell through a jump host.
sshore forward .:8080 work:3000     # Reach the remote service on local port 8080.
sshore ping work:                   # Measure SSH latency.
```

## Shells — Start, detach and resume

```sh
sshore sh work:build         # Open a temporary shell.
sshore sh work:build --pin   # Start a shell that survives disconnection.
sshore attach work:build     # Resume a detached shell.
```

A top-level `sh` ends when its starting connection closes. Inside a shell,
interactive `sh` creates a child and gives it the keyboard. Its parent waits.
Each tree has one foreground shell. Independent roots can be used in parallel.

From an ordinary terminal or the root prompt, `attach` opens an idle root.
Inside a shell on the same daemon, `attach :NAME` enters a detached direct
child. If NAME is an idle root, it becomes a child here, along with its
subtree. Follow the branches one level at a time. A nested `sh gpu:build`
opens a child over SSH. Its parent can re-enter it with `attach gpu:build`
while that connection lives. Other remote roots can be opened from a separate
terminal.

`detach` pins the current shell and returns to its parent. Its position in
the tree stays the same. A duration such as `detach 2h` sets a deadline.
A deadline limits retention after the owner disappears. A live parent still
keeps its child after the pin expires. With no duration, or with `sh --pin`,
the pin lasts until exit. When a parent ends, each pinned child becomes an
independent root with its descendants.
Typing `exit` in a shell ends that shell even if pinned.

Press **Ctrl+B** followed by `c` for a nested shell, `d` to detach, or
`:` for the command prompt. Leaving the root by detach or exit also opens
that prompt. It shows the exit or detach notice and the shell tree. Use
`attach` or `sh` to continue, or type `exit` at the root prompt to leave
sshore. When an exiting nested shell leaves pinned children, the prompt shows
which children become independent roots. Press Esc or type `exit` to return
to the previous shell.

With no selector, `sshore attach` resumes the most recently detached eligible
local shell. With `sshore attach work:`, the remote host identifies its idle
roots. Your detach history selects the most recent match. If several roots
remain and none matches your history, choose one at the prompt. From an ordinary terminal,
a bare hexadecimal ID or unique prefix can select a local shell or a remote
shell with a saved direct SSH connection. Use `HOST:ID` to disambiguate.
Scrollback is not replayed on attach.

### Find and manage tasks

```sh
sshore pstree               # Shell tree (ps).
sshore list                 # Shell, mount and forward details (ls).
sshore list --json          # Machine-readable inventory.
```

`pstree` shows shell names, short IDs and states. A leading `*` marks your
current shell. `waiting` means a shell has handed off the keyboard. A remote
observation shows its host and recorded state.

```text
dev (111111)  waiting
├── * edit (222222)  active
╰── build (333333) on gpu  detached
```

`list` includes working directories, pin deadlines, remote observation times,
and resource details. `PARENT` shows the parent shell's short ID, or `-` for a
root or a resource. For remote observations, it shows the last recorded parent.
Connection hosts and endpoints appear in `DETAIL`. For example,
`local:3000 <- gpu:8080` forwards connections from the listener on `gpu` to
the local service on port 3000.

Use a task name or ID from the list to attach, rename or end a shell.
For example, `sshore kill work:build` ends the shell and can discard unsaved work.

## copy-id — Install an SSH public key for remote login

Run `sshore copy-id work` to add a configured SSH public key to the remote
account's `~/.ssh/authorized_keys`. The account must allow SSH command execution.
The host needs `flock`, `base64` and `tail`. You can run this before installing
sshore on the host. Existing entries for the same key keep their restrictions.

## scp (+lrzsz) — Transfer files from either end of an SSH session

```sh
sshore cp report.zip work:/tmp/           # Upload a file.
sshore cp work:/tmp/report.zip ./         # Download a file.
sshore cp report.zip origin:              # From an attached remote shell, send a file to the client.
```

`work:VALUE` selects a path or port on the SSH host. `here:` (or `.:`) selects
the machine running the command. A bare file path also belongs to that machine.
On Windows, use `C:\dir\file` or `here:C:/dir/file` for a local drive path.

Copy accepts regular files and refuses to overwrite existing destinations.
Inside an attached remote shell, `origin:` refers to the attached client.
Review the transfer request on that client and choose the local path.

## sftp (sshfs) — Mount a remote directory locally

```sh
sshore mount work:/srv/project .: --read-only
```

Mounts use SFTP and require FUSE on Linux, [WinFsp](https://winfsp.dev/) on
Windows, or [macFUSE](https://macfuse.github.io/) with its kernel extension on
Apple Silicon Macs using the experimental source build. The mount command
above prints the chosen mountpoint. Linux and macOS use `~/mnt/NAME`, with the
registered task name for a background mount and a fresh six-character name for
`--foreground`. Windows chooses a free drive letter. Close open files before
stopping a mount with the kill command printed at startup.

## Help and troubleshooting

- Run `sshore --tldr` for examples or `sshore COMMAND --help` for options.
- For an active mount or forward, `sshore list --json` includes its log path.

## Project documents

[Architecture and development](SSHORE.md) · [Security](SECURITY.md) ·
[Glossary](GLOSSARY.md) · [Contributor rules](AGENTS.md)

## Acknowledgements

Inspired by [dtach](https://github.com/crigler/dtach),
[sshping](https://github.com/spook/sshping) and [trzsz](https://github.com/trzsz/trzsz).

[MIT license](LICENSE) · [Third-party notices](THIRD_PARTY_LICENSES.txt)
