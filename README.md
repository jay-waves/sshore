# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)
[![Linux amd64 and Windows amd64](.github/badges/platforms.svg)](#os-and-shell-support)

Start a shell on this computer or over SSH, detach, and pick up where you left
off. Copy files, forward TCP or UDP ports, and mount remote directories.

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

The client deploys the matching server over SSH and SFTP when a remote
operation needs it.

## OS and shell support

| Shell on OS (amd64) | Shell / reattach | File copy | SFTP mount | TCP / UDP forward |
| --- | --- | --- | --- | --- |
| bash / sh / zsh / fish on Linux | ✓ | ✓ | ✓ | ✓ |
| PowerShell / cmd on Win11 or Server 2025 and newer | ✓ | ✓ | ✓ | ✓ |

Managed remote shells require SSH TCP forwarding to loopback.
See the [development notes](docs/ARCHITECTURE.md#terminal-and-mux-design-scope)
for experimental Apple Silicon support.

## ssh — Run remote commands and forward ports

Use SSH host aliases from `ssh_config`.

```sh
sshore sh jay-waves@pi  # Log in as a specific SSH user.
sshore sh pi -J jump    # Open a shell through a jump host.
sshore forward pi:3000 :8080  # Reach the remote service on local port 8080.
sshore ping pi                # Measure SSH latency.
sshore fwd --udp pi:53 1053   # Forward a remote DNS service to local port 1053.
```

Run `sshore fwd :9090 ::9091` from an attached remote shell to request
forwarding its port 9090 to port 9091 on the client. Use `sshore fwd 100 200 3000 ::`
to request a batch that keeps the same port numbers. Approve the request on the client.

### ssh-exec

`sshore sh HOST -- COMMAND` joins the words after `--` for the remote shell.
It runs without a remote PTY or sshore server.

```sh
sshore sh pi -- uname -a
sshore sh pi -- 'ls -l "a b.txt"'
```

### start — Start a shell or program in a terminal

`sshore start HOST -- PROGRAM [ARG...]` preserves each argument and opens a
launch review before starting the program in a PTY. `go` is an alias for
`start`. Use `--pin` to keep the task running after disconnect.

```sh
sshore start pi -- vim 'a b.txt'  # Open one file whose name contains a space.
sshore start pi --pin            # Open a shell that survives disconnect.
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
sshore attach pi  # Match a known shell name first, then try the SSH host.
sshore shell pi   # Start a new shell on the SSH host.
```

Use a task ID when a shell name is ambiguous.

### Find and manage tasks

Run `sshore list` and `sshore pstree` for task details.
Use `sshore kill ID` or `sshore kill NAME` to stop a shell, mount or forward.
Killing a shell can discard unsaved work.

## copy-id — Install an SSH public key for remote login

Run `sshore copy-id pi` to add a configured public key to the remote
account's `~/.ssh/authorized_keys`. Existing entries keep their restrictions.

## scp (+lrzsz) — Transfer files from either end of an SSH session

```sh
sshore cp report.zip pi:/tmp/           # Upload a file.
sshore cp pi:/tmp/report.zip ./         # Download a file.
sshore cp -r ./project pi:/tmp/         # Upload a directory tree.
sshore cp --lf : pi:                   # Pick both paths.
sshore cp report.zip ::               # Send to the client from an attached shell.
```

A bare path belongs to the machine running the command. Use `:` for an
explicit local endpoint and `[IPv6]:PATH` for an IPv6 host.
An empty local path uses the current directory
and an empty SSH path uses the remote home. Add `--lf` to pick omitted paths.
Recursive copy includes empty directories, rejects symbolic links, and keeps
existing files. `::` refers to the attached client and requests its approval.
On Windows, use `C:\dir\file` or `:C:/dir/file` for a local drive path.

## sftp (+fuse) — Mount a remote directory locally

```sh
sshore mount pi:/srv/project : --read-only
sshore mount winhost:E:/project :
sshore mount pi: :
sshore mount --lf pi: :
```

An empty remote path mounts the remote home and an empty local mountpoint
allocates one automatically. Add `--lf` to pick omitted paths. Use `:` to
keep automatic mountpoint allocation with `--lf`.

Windows hosts serve directories on NTFS and ReFS volumes; other filesystems are rejected.
Mounts require FUSE on Linux, [WinFsp](https://winfsp.dev/) on Windows, or
the [macFUSE](https://macfuse.github.io/) kernel extension on experimental
macOS builds. Use `sshore kill ID` to unmount.

## TUI

### `lf` — Browse files

```sh
sshore lf                      # Browse the local current directory
sshore lf work                 # Browse the remote home directory
sshore lf : pi:                # Browse locally with copy and mount enabled
sshore lf work:/srv/project :  # Browse remotely with copy and mount enabled
```

| Key | Action |
| --- | --- |
| `o` | Open a local file or directory in the system default app |
| `e` / `i` | Edit or view local text with `$EDITOR` / `$PAGER` |
| `l` | Enter a directory or open a local file |
| `y` | Copy the selected path |
| `s` | Open a shell |
| `c` / `m` | Copy / mount when both endpoints are set |
| `q` | Quit |

Press `Enter` to confirm a path in the copy or mount picker.
System open uses `xdg-open` on Linux, `open` on macOS, and `ShellExecute` on Windows.

### `lp` — Browse TCP and UDP ports

```sh
sshore lp          # List local ports
sshore lp work     # List remote ports
sshore lp work: :  # Select a remote service and listen locally
sshore lp : work:  # Select a local service and listen remotely
```

Use `/` to filter, `r` to refresh, `s` to sort by port or process, `y` to copy
the selected address, and `q` to quit. With two endpoints, `Enter` chooses
the listening port. Some process names may require administrator privileges.

## Advanced usage

- Use `-J` to connect through a jump host, as in
  `sshore start pi -J jump -- nvim`.
- Use `-` to pipe a single file over SSH. Upload to a new remote filename with
  `gzip -c report.txt | sshore cp - pi:/tmp/report.txt.gz`, or download with
  `sshore cp pi:/tmp/report.txt.gz - | gzip -dc`.
- The terminal supports OSC 52 passthrough. Both browsers use OSC 52 for `y`.
- `Ctrl+B` shortcuts:
  - `Ctrl+B c` opens a nested shell.
  - `Ctrl+B d` pins and detaches the current shell.
  - `Ctrl+B s` or `Ctrl+B :` opens the command prompt.
  - `Ctrl+B r` redraws the terminal.

## Help and troubleshooting

- Run `sshore --tldr` for examples or `sshore COMMAND --help` for options.
- Each host's account daemon records shell disconnects, retention and other
  background diagnostics in `~/.sshore/daemon/local.log`.

## Project documents

[Architecture](docs/ARCHITECTURE.md) · [Security](docs/SECURITY.md) ·
[Approval and console](docs/approval.md) · [Mount implementation](docs/mount.md) ·
[Glossary](docs/glossary.md) · [Contributor rules](AGENTS.md)

## Acknowledgements

Inspired by [dtach](https://github.com/crigler/dtach),
[sshping](https://github.com/spook/sshping), [trzsz](https://github.com/trzsz/trzsz),
[sshoq](https://github.com/h4sh5/sshoq), [lf](https://github.com/gokcehan/lf),
and [lsoff](https://github.com/yutat23/lsoff).

[MIT license](LICENSE.txt) · [Third-party notices](docs/THIRD_PARTY_LICENSES.txt)
