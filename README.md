# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)
[![Linux amd64 and Windows amd64](.github/badges/platforms.svg)](#os-and-shell-support)

Start a shell on this computer or over SSH, detach, and pick up where you left
off. You can also copy files, forward ports and mount remote directories.

## Install the client

Download a [release package](https://github.com/jay-waves/sshore/releases), or run:

**Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh | sh
```

**Windows** — PowerShell 5.1 or 7:

```powershell
irm https://raw.githubusercontent.com/jay-waves/sshore/main/install.ps1 | iex
```

The scripts install the local client. On the first managed SSH connection, the
client automatically deploys the matching server over SSH and SFTP.

## OS and shell support

| Shell on OS (amd64) | Shell / reattach | File copy | SFTP mount | TCP forward |
| --- | --- | --- | --- | --- |
| bash / sh / zsh / fish on Linux | ✓ | ✓ | ✓ | ✓ |
| pwsh / cmd on Win11 | ✓ | ✓ | ✓ | ✓ |

Managed shells and their resource routes also require SSH TCP forwarding to
loopback.
See the [development notes](docs/ARCHITECTURE.md#terminal-and-mux-design-scope)
for experimental Apple Silicon support.

## ssh — Run remote commands and forward ports

Use SSH host aliases from `ssh_config`.

```sh
sshore sh jay-waves@raspberrypi  # Log in as a specific SSH user.
sshore sh raspberrypi -J jump    # Open a shell through a jump host.
sshore forward raspberrypi:3000 .:8080  # Reach the remote service on local port 8080.
sshore ping raspberrypi:               # Measure SSH latency.
```

Use `sshore fwd SOURCE TARGET` to mirror a TCP service. For example, run
`sshore fwd .:9090 origin:9091` in a remote shell to access its port 9090
at `127.0.0.1:9091` on your computer. Approve the target port on your computer
to resume the remote shell.

### ssh-exec

`sshore sh HOST -- COMMAND` runs once over SSH without a remote PTY or a
remote sshore installation. Like OpenSSH, it joins the words after `--`
with spaces and lets the remote shell interpret the result.

```sh
sshore sh raspberrypi -- uname -a
sshore sh raspberrypi -- vim 'a b.txt'       # Vim receives two filenames: a and b.txt.
sshore sh raspberrypi -- 'vim "a b.txt"'     # Vim receives one filename, with no PTY.
```

### new — Start a program in a terminal

`sshore new HOST -- PROGRAM [ARG...]` starts the program directly in a PTY
and preserves each argument. Omit `-- PROGRAM` to open the default shell.
Use `--pin` to keep the task running after disconnect.

```sh
sshore new raspberrypi -- vim 'a b.txt'  # Open one file whose name contains a space.
sshore new raspberrypi --pin            # Open a shell that survives disconnect.
```

## mux — Start, detach and resume

`sshore detach` pins the current shell until it exits and returns you to its
parent shell. If there is no parent shell, it opens the sshore command prompt
(`sshore>`). You can also open the command prompt from an active shell with
`Ctrl+B s`.

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

### Resume a detached shell

```sh
sshore attach              # Resume the most recently detached eligible local shell.
sshore attach 7b3a91       # Resume a detached shell by task ID.
sshore attach raspberrypi  # Match a known shell name first, then try the SSH host.
sshore shell raspberrypi   # Start a new shell on the SSH host.
```

Use a task ID when a shell name is ambiguous.

### Find and manage tasks

Run `sshore list` and `sshore pstree` for task details.
Use `sshore kill ID` or `sshore kill NAME` to stop a shell, mount or forward.
Killing a shell can discard unsaved work.

## copy-id — Install an SSH public key for remote login

Run `sshore copy-id raspberrypi` to add a configured SSH public key to the remote
account's `~/.ssh/authorized_keys`. The account must allow SSH command execution.
The host needs `flock`, `base64` and `tail`. You can run this before installing
sshore on the host. Existing entries for the same key keep their restrictions.

## scp (+lrzsz) — Transfer files from either end of an SSH session

```sh
sshore cp report.zip raspberrypi:/tmp/           # Upload a file.
sshore cp raspberrypi:/tmp/report.zip ./         # Download a file.
sshore cp -r ./project raspberrypi:/tmp/         # Upload a directory tree.
sshore cp report.zip origin:              # From an attached remote shell, send a file to the client.
```

`raspberrypi:VALUE` selects a path or port on the SSH host. `here:` (or `.:`) selects
the machine running the command. A bare file path also belongs to that machine.
On Windows, use `C:\dir\file` or `here:C:/dir/file` for a local drive path.

Copy accepts regular files. Use `-r` to upload or download a directory tree,
including empty directories. Recursive copy rejects symbolic links. Existing
directories can receive new entries, and existing files are never overwritten.
Leave a path empty to choose it in the three-column picker after SSH connects.
For example, `sshore cp here: raspberrypi:` selects a local file, then a remote
destination directory. Use the arrow keys or `hjkl` to browse and `q` to cancel.
Inside an attached remote shell, `origin:` refers to the attached client.
Review the transfer request on that client and choose the local path. With
`cp -r`, approve and choose a local directory for each requested tree.

## sftp (+fuse) — Mount a remote directory locally

```sh
sshore mount raspberrypi:/srv/project .: --read-only
sshore mount winhost:E:/project .:
sshore mount raspberrypi: here: --read-only
```

Use `HOST:` to browse from the remote home, `HOST:~` to select it directly,
and `here:` to choose the local mountpoint. Use `.:` for automatic allocation.

Windows hosts serve directories on NTFS and ReFS volumes; other filesystems are rejected.
Mounts use SFTP and require FUSE on Linux, [WinFsp](https://winfsp.dev/) on
Windows, or the [macFUSE](https://macfuse.github.io/) kernel extension on
experimental macOS builds. The command prints the mountpoint and how to stop
it. Close open files before unmounting.

## tui — Browse files and ports

`sshore lf` opens the current directory. Use `sshore lf HOST` for the remote home directory
or `sshore lf HOST:/path` for a specific directory. Press `s` to open a shell, `c` to copy,
`m` to mount, or `f` to browse ports on the same host.

`sshore lp [HOST]` lists local or remote TCP and UDP ports. Use the arrow keys to select
a port or `/` to filter. Press Enter to review the target port, then Enter to start forwarding.
Press `q` to leave either browser.

## Advanced usage

- Use `-J` to connect through a jump host, as in
  `sshore new raspberrypi -J jump -- nvim`.
- OSC 52 clipboard passthrough.
- `Ctrl+B` shortcuts:
  - `Ctrl+B c` opens a nested shell.
  - `Ctrl+B d` pins and detaches the current shell.
  - `Ctrl+B s` or `Ctrl+B :` opens the command prompt.
  - `Ctrl+B r` redraws the terminal.

## Help and troubleshooting

- Run `sshore --tldr` for examples or `sshore COMMAND --help` for options.
- Each host's account daemon records shell disconnects, retention and other
  background diagnostics in `~/.sshore/daemon/local.log`.
- Status and diagnostic messages show local time as `MM-DD HH:mm` on screen,
  plus the task ID when available and severity. Log files include the year and seconds.

## Project documents

[Architecture](docs/ARCHITECTURE.md) · [Security](docs/SECURITY.md) ·
[Approval and console](docs/approval.md) · [Mount implementation](docs/mount.md) ·
[Glossary](docs/GLOSSARY.md) · [Contributor rules](AGENTS.md)

## Acknowledgements

Inspired by [dtach](https://github.com/crigler/dtach),
[sshping](https://github.com/spook/sshping), [trzsz](https://github.com/trzsz/trzsz)
and [sshoq](https://github.com/h4sh5/sshoq). The file picker is inspired by
[lf](https://github.com/gokcehan/lf).

[MIT license](LICENSE.txt) · [Third-party notices](docs/THIRD_PARTY_LICENSES.txt)
