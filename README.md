# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)
[![Linux amd64 and Windows amd64](.github/badges/platforms.svg)](#os-and-shell-support)

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

Installers add the executable directory to the user PATH. Linux updates `.profile`,
`.bashrc` and `.zshenv`; Windows updates the user environment. New sessions are
needed, and SSH server environment settings must allow that PATH to take effect.

Managed connections require matching releases. `sshore bootstrap work` installs
your exact release on the Linux host `work`. `sshore bootstrap` installs the
latest release locally. Either command ends the target account's tasks, including
pinned shells, and clears its sshore identity, history and cached releases.

## OS and shell support

| Platform (amd64) | Local shells | Remote hosting |
| --- | --- | --- |
| Linux | bash, sh, zsh, fish | Supported |
| Windows 11 / Server 2025 or newer | PowerShell 7, cmd | SFTP mounts |

Managed shells require the host's SSH server to allow TCP forwarding to
loopback. See the [development notes](SSHORE.md#terminal-and-mux-design-scope)
for experimental Apple Silicon support.

## ssh — Run remote commands and forward ports

Use SSH host aliases from `ssh_config`. Pass SSH options after `--`, including
`-F FILE` for a different config or `-F none` for built-in defaults. Configure
TCP listeners with `sshore forward`. Forwarding requires the same sshore release
on the Linux destination, including with `--foreground`.
`sshore exec` runs directly over SSH without starting the account daemon.

```sh
sshore exec work 'uname -a'      # Run one command on work.
sshore shell work -- -J jump    # Open a shell through a jump host.
sshore forward .:8080 work:3000  # Reach the remote service on local port 8080.
sshore ping work:               # Measure SSH latency.
```

## Shells — Start, detach and resume

`sshore shell` (`sh`) opens a shell on the machine running the command.
`sshore shell work` opens one over SSH. Each shell receives an automatic ID.

`sshore attach` resumes the most recently detached eligible local shell.
Use `sshore attach ID` with the ID shown by `sshore pstree`.

Detach a child, exit its parent, then resume it under a new parent.
The IDs below are illustrative, and tree output is abbreviated.

```text
$ sshore shell
a1b2c3$ sshore shell
7b3a91$ sshore detach
a1b2c3$ sshore pstree
  * a1b2c3  active
  ╰── 7b3a91  detached

a1b2c3$ exit
sshore> exit
$ sshore pstree
  7b3a91  detached

$ sshore shell
e4f567$ sshore attach 7b3a91
7b3a91$ sshore pstree
  e4f567  waiting
  ╰── * 7b3a91  active
```

While the original parent is alive, only it can reattach its child. Each tree
has one foreground shell.

### Select a shell over SSH

In `HOST:SELECTOR`, the left side selects the SSH destination, including the
login account, and the right side is resolved by that account's daemon.
This lets two accounts use the same shell name and lets you select a remote
task without relying on a saved connection.

| Command | What the part after `:` means |
| --- | --- |
| `sshore shell work` | Create a shell on `work` with an automatic ID. |
| `sshore attach work:7b3a91` | Resume the shell whose ID uniquely starts with `7b3a91` on `work`. The ID stays the same across renames. |
| `sshore attach work:` | Let the remote daemon supply eligible detached roots and use detach history to choose one. An explicit colon also works for hexadecimal SSH aliases such as `db:`, where bare `db` is parsed as an ID. |

From the parent shell, `sshore attach 7b3a91` can also resume its direct
SSH child. If the ID prefix matches more than one shell, use a longer prefix,
`HOST:ID` for a remote child or `:ID` for a shell on the current daemon.

For optional names, `sshore shell HOST:NAME` creates a named shell and
`sshore attach HOST:NAME` selects it. Use `:NAME` or `here:NAME` for a
local named shell.

### Find and manage tasks

Run `sshore list` for task details.
Use `sshore kill ID` or `sshore rename ID NEW` for shells, mounts and forwards.
`HOST:ID` selects the managing SSH account explicitly. Killing a shell can
discard unsaved work.

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
sshore mount winhost:E:/project .:
```

The SSH account must find the matching sshore release on its PATH.
Windows hosts serve directories on NTFS and ReFS volumes; other filesystems are rejected.
Mounts use SFTP and require FUSE on Linux, [WinFsp](https://winfsp.dev/) on
Windows, or the [macFUSE](https://macfuse.github.io/) kernel extension on
experimental macOS builds. The command prints the mountpoint and how to stop
it. Close open files before unmounting.

## Help and troubleshooting

- Run `sshore --tldr` for examples or `sshore COMMAND --help` for options.
- Each host's account daemon records shell disconnects, retention and other
  background diagnostics in `~/.sshore/daemon/local.log`.
- Status and diagnostic messages show local time, task ID when available, and severity.

## Project documents

[Architecture and development](SSHORE.md) · [Security](SECURITY.md) ·
[Glossary](GLOSSARY.md) · [Contributor rules](AGENTS.md)

## Acknowledgements

Inspired by [dtach](https://github.com/crigler/dtach),
[sshping](https://github.com/spook/sshping) and [trzsz](https://github.com/trzsz/trzsz).

[MIT license](LICENSE) · [Third-party notices](THIRD_PARTY_LICENSES.txt)
