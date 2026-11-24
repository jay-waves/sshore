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

Managed connections require matching releases. `sshore bootstrap work` installs
your exact release on the Linux host `work`. `sshore bootstrap` installs the
latest release locally. Either command ends the target account's tasks, including
pinned shells, and clears its sshore identity, history and cached releases.

## OS and shell support

| Platform (amd64) | Local shells | Remote hosting |
| --- | --- | --- |
| Linux | bash, sh, zsh, fish | Supported |
| Windows 11 / Server 2025 or newer | PowerShell 7, cmd | — |

Managed shells require the host's SSH server to allow TCP forwarding to
loopback. See the [development notes](SSHORE.md#terminal-and-mux-design-scope)
for experimental Apple Silicon support.

## ssh — Run remote commands and forward ports

Use SSH host aliases from `ssh_config`. Pass SSH options after `--`, including
`-F FILE` for a different config or `-F none` for built-in defaults. Configure
TCP listeners with `sshore forward`.

```sh
sshore exec work 'uname -a'      # Run one command on work.
sshore shell work: -- -J jump   # Open a shell through a jump host.
sshore forward .:8080 work:3000  # Reach the remote service on local port 8080.
sshore ping work:               # Measure SSH latency.
```

## Shells — Start, detach and resume

Use `sshore shell :NAME` for a local shell or `sshore shell HOST:NAME` over SSH.
`sh` is an alias. Start with `--pin` to retain a shell if its connection or
parent ends.

1. Run `sshore shell :dev`, then run `sshore shell :build` inside `dev`.
   `dev` waits while `build` has the keyboard. Run `sshore detach` in `build`
   or press **Ctrl+B**, then `d`. This pins `build` and returns you to `dev`.

   ```text
   * dev  active
   ╰── build  detached
   ```

   While `dev` lives, only its session can enter `build`. To try it, run
   `sshore attach :build` from `dev`, then detach again. Each tree has one
   foreground shell, and independent roots can run in parallel.

2. Type `exit` in `dev`. The pinned `build` survives as an independent root
   with its descendants. An unpinned child ends with its parent. Type `exit`
   again at the `sshore` prompt to return to your terminal.

   ```text
   build  detached
   ```

3. Run `sshore shell :review`, then run `sshore attach :build` inside `review`.
   Attaching an idle root on the same daemon grafts its whole subtree under
   the current shell and hands it the keyboard.

   ```text
   review  waiting
   ╰── * build  active
   ```

See `sshore attach --help` for selectors. Press **Ctrl+B**, then `:` for the
sshore command prompt and `detach DURATION` to set a pin deadline.

### Find and manage tasks

```sh
sshore pstree               # Shell tree (ps).
sshore list                 # Shell, mount and forward details (ls).
sshore list --json          # Machine-readable inventory.
```

Use a task name or short ID to attach, rename or end a shell. A leading `*`
marks your current shell. `sshore kill work:build` ends the shell and can
discard unsaved work. See [inventory and diagnostics](SSHORE.md#inventory-and-diagnostics)
for fields and forwarding direction.

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
Windows, or the [macFUSE](https://macfuse.github.io/) kernel extension on
experimental macOS builds. The command prints the mountpoint and how to stop
it. Close open files before unmounting.

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
