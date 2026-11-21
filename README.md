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

Both clients can connect to Linux over SSH. sshore reads host settings through
the system OpenSSH client. For managed shells, the host's SSH server must allow
TCP forwarding to loopback. See [development notes](SSHORE.md#terminal-and-mux-design-scope)
for the experimental Apple Silicon client, which supports local shells using
the login shell selected by `$SHELL`.

## ssh — Run remote commands and forward ports

```sh
sshore exec work 'uname -a'         # Run one command on work.
sshore spawn work: -- -J jump       # Open a remote shell through a jump host.
sshore forward .:8080 work:3000     # Reach the remote service on local port 8080.
sshore ping work:                   # Measure SSH latency.
```

## tmux — Start, detach and resume shell sessions

```sh
sshore launch work:build     # Start a remote shell named build.
# Press Ctrl+B, then d to detach.
sshore attach work:build     # Return to the same shell.
```

Use `launch` for work that should survive a disconnect. Use `spawn` for a shell
that should end when its original parent exits. A remote spawn also ends if its
lifetime SSH connection times out. Reattaching preserves the original lifetime.
Each shell has one terminal. Scrollback is not replayed on attach.

For a local shell, run `sshore launch :build`. With no selector, `sshore attach`
resumes the most recently detached local shell. Use `sshore attach work:` for
the most recently detached shell on that host. A bare task ID or unique prefix
can select a local shell or a remote shell with a saved direct SSH connection.
If the ID is ambiguous, use a longer prefix or specify `HOST:ID`.

While attached, press **Ctrl+B** followed by `d` to detach, `c` to spawn a
nested shell, or `:` to open the command prompt. Type `help` there to explore
commands and Esc to return to the shell.

### Find and manage tasks

```sh
sshore list                 # Local tasks and incoming resources.
sshore list work:           # This client's last known tasks on work, even offline.
sshore list --json          # Machine-readable local inventory.
sshore pstree               # Local ownership and attachments. *current marks this shell.
```

Use a task name or ID from the list to attach, rename or end a shell.
For example, `sshore kill work:build` ends the shell and can discard unsaved work.

## copy-id — Install an SSH public key for remote login

Run `sshore copy-id work` to add a configured SSH public key to the remote
account's `~/.ssh/authorized_keys`.

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
