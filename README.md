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

| Shell on OS (amd64) | Shell / reattach | File copy | SFTP mount | TCP forward |
| --- | --- | --- | --- | --- |
| bash / sh / zsh / fish on Linux | ✓ | ✓ | ✓ | ✓ |
| pwsh / cmd on Win11 | ✓ | ✓ | ✓ | ✓ |

Managed shells and their resource routes require the host's SSH server to
allow TCP forwarding to loopback.
See the [development notes](docs/ARCHITECTURE.md#terminal-and-mux-design-scope)
for experimental Apple Silicon support.

## ssh — Run remote commands and forward ports

Use SSH host aliases from `ssh_config`. Pass SSH options after `--`, including
`-F FILE` for a different config or `-F none` for built-in defaults. Port
forwarding requires the same sshore release on the destination.
`sshore exec` runs directly over SSH without starting the account daemon.

```sh
sshore exec work 'uname -a'      # Run one command on work.
sshore shell work -- -J jump    # Open a shell through a jump host.
sshore forward work:3000 .:8080  # Reach the remote service on local port 8080.
sshore ping work:               # Measure SSH latency.
```

Use `sshore fwd SOURCE TARGET` to mirror a TCP service. For example, run
`sshore fwd .:9090 origin:9091` in a remote shell to access its port 9090
at `127.0.0.1:9091` on your computer. Approve the target port on your computer
to resume the remote shell.

## mux — Start, detach and resume

Detach a child, do something, then resume it. 

```text
$ sshore shell
a1b2c3$ sshore shell
7b3a91$ sshore detach
a1b2c3$ sshore pstree
local
╰── * shell/a1b2c3  active
    ╰── shell/7b3a91  detached

a1b2c3$ exit
sshore> exit
$ sshore pstree
local
╰── shell/7b3a91  detached

$ sshore shell
e4f567$ sshore attach 7b3a91
7b3a91$ sshore pstree
local
╰── shell/e4f567  waiting
    ╰── * shell/7b3a91  active
```

While the original parent is alive, only it can reattach its child. Each tree
has one foreground shell.

### Attach recent jobs

```sh
sshore shell work          # Start a new shell.
sshore attach work         # Resume a detached shell, by LRU
sshore attach work:        # The same, `work` will parsed as a HOST name.
sshore attach 7b3a91       # Resume a detached shell, by ID
sshore attach work:7b3a91  # Resume a detached shell, by HOST:ID.
```

If the selected shell is unavailable during SSH setup, sshore can open a new
one and print its ID.

### Find and manage tasks

Run `sshore list` and `sshore pstree` for task details.
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

## sftp (+fuse) — Mount a remote directory locally

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
- Status and diagnostic messages show local time as `MM-DD HH:mm` on screen,
  plus the task ID when available and severity. Log files include the year and seconds.

## Project documents

[Architecture](docs/ARCHITECTURE.md) · [Security](docs/SECURITY.md) ·
[Glossary](docs/GLOSSARY.md) · [Contributor rules](AGENTS.md)

## Acknowledgements

Inspired by [dtach](https://github.com/crigler/dtach),
[sshping](https://github.com/spook/sshping), [trzsz](https://github.com/trzsz/trzsz)
and [sshoq](https://github.com/h4sh5/sshoq).

[MIT license](LICENSE) · [Third-party notices](docs/THIRD_PARTY_LICENSES.txt)
