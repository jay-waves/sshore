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

Install the matching release on your Linux host with `sshore install work`,
where `work` is a hostname or SSH config alias. Managed connections require
the same full release version on both ends. When updating a host with active
tasks, review the task list before confirming replacement.

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

A top-level shell started with `sh` ends when its starting client disconnects.
A nested shell follows its parent. Start with `--pin` to retain a shell until it
exits. `sshore detach` keeps the current shell until it exits and leaves its
terminal. `sshore detach 2h` keeps it for two hours. When a pin expires, a shell
whose original owner has already ended is removed once it is detached. Pinned
children survive parent exit and retain any descendants still following them.

While attached, press **Ctrl+B** followed by `c` for a nested shell, `d` to pin
and detach, or `:` for the command prompt and the current host's `pstree`.
The prompt shows the host, for example `sshore on work>`. Local shells show
`sshore on local>`. Run `detach 30m` there to set a deadline.

Leaving a nested shell returns to its parent. At the root, detaching or
running `exit` in the shell opens the command prompt. Use `attach build` or
`sh` there to continue, or type `exit` to leave sshore.

For a local shell, run `sshore sh :build`. With no selector, `sshore attach`
resumes the most recently detached local shell. Use `sshore attach work:` for
the most recent detach on that host. A bare task ID or unique prefix can select
a local shell or a remote shell with a saved direct SSH connection. If the ID is
ambiguous, use a longer prefix or specify `HOST:ID`.

Each shell has one terminal. Scrollback is not replayed on attach.

### Find and manage tasks

```sh
sshore list                 # Local tasks, incoming resources and last seen remote shells.
sshore list --json          # The same inventory as JSON.
sshore pstree               # Shell ownership and attachment states. * marks this shell.
```

In `pstree` (or `ps` at the command prompt), a nested shell stays under its
original owner when either shell detaches. `active` means the shell has an
attached client. `*` identifies the shell using your terminal.

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
above prints the chosen mountpoint. Close open files
before stopping a mount with the kill command printed at startup.

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
