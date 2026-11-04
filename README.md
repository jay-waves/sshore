# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)

A small SSH command-line tool: disconnect while your remote shell keeps running, then reconnect to the same process. Supports multiple named sessions, with one attached client per session.

Written in Go and built on system OpenSSH. The server runs on Linux; clients run on Linux and Windows. smux protocol v2 carries separate control, terminal, and file streams over one SSH connection. File transfers use a custom protocol, without SFTP or trzsz compatibility. Transferring files does not start another SSH connection or ask for your SSH password again.

## Build and install

Prebuilt binaries do not require Go. After a successful CI run, download Linux or Windows packages for amd64 or arm64 from the [Actions artifacts](https://github.com/jay-waves/sshore/actions). Pushing a `v*` tag publishes a [release](https://github.com/jay-waves/sshore/releases) after tests pass, including archives and SHA-256 checksums.

Building from source requires Go 1.25+. The local machine needs an OpenSSH client (`ssh` or `ssh.exe`), and the remote machine needs a working SSH server.

```sh
git clone https://github.com/jay-waves/sshore.git
cd sshore
go build -o dist/sshore .

# Cross-compile the Windows client on Linux.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -o dist/sshore.exe .
```

Install the Linux binary on the remote PATH, for example as `~/.local/bin/sshore`, and install the appropriate binary locally. Use the same version on both ends; connections check the protocol version. No root access or additional listening network port is required.

Version 0.2 uses a new multiplexing protocol that is incompatible with 0.1. Before upgrading, attach to any running 0.1 sessions with the old client and exit their shells, then replace both binaries. Running session processes cannot upgrade their protocol in place.

```sh
# Install on the Linux server.
install -Dm755 dist/sshore "$HOME/.local/bin/sshore"
```

A noninteractive SSH connection may not include `~/.local/bin` in PATH. Specify an absolute remote path if needed:

```sh
sshore myserver --server /home/alice/.local/bin/sshore
```

Run the Windows client in a modern console with VT support, such as Windows Terminal. The Windows binary is a client; session and file commands run inside the connected Linux shell.

## Usage

```sh
sshore user@host                 # Create or attach to the main session.
sshore myserver -s build         # Create or attach to the build session.
sshore myserver list             # List sessions, shell PIDs, and attachment status.
sshore myserver -J jump -p 2222   # Pass SSH connection options.
sshore myserver --download-dir ./downloads  # Save downloads automatically.
```

sshore uses system OpenSSH configuration, host keys, and authentication settings. Supported SSH options are `-p`, `-i`, `-J`, `-F`, `-l`, `-o`, `-v`, `-4`, and `-6`. Put complex settings in `~/.ssh/config`. Not every OpenSSH option is forwarded. If a host alias conflicts with a built-in command, configure another SSH alias for that host.

Session names contain 1-48 ASCII letters, digits, underscores, or hyphens, starting with a letter or digit. An attached session rejects a second client; clients cannot take over a connection or share input.

Inside the remote shell:

```sh
sshore detach                   # Disconnect and keep the session running.
sshore upload                   # Choose a local file to upload to the current directory.
sshore upload /tmp/uploads       # Upload to a specific remote directory.
sshore download ./report.zip     # Choose a local destination for the download.
sshore download a.zip b.log c.png # Download a batch, with up to two concurrent transfers.
exit                            # End the shell and its session.
```

Common options:

| Option | Purpose |
| --- | --- |
| `-s, --session NAME` | Persistent session name; defaults to `main` |
| `--server PATH` | Remote sshore executable; defaults to a PATH lookup |
| `--download-dir DIRECTORY` | Existing local directory for automatic downloads without individual path prompts; never overwrites files |
| `-q, --quiet` | Hide periodic progress while retaining prompts, completion messages, and errors |
| `--no-escape` | Forward Ctrl-] to the remote application; use `sshore detach` to disconnect |
| `-h, --help` | Show help; accepted before or after connection arguments |
| `-V, --version` | Show the version |

Use `sshore help download` or `sshore download --help` for command help. Put `--` before paths that begin with a hyphen, for example `sshore download -- -report.txt`. Invalid arguments return exit code `2`; other failures return a nonzero code. A normal remote shell exit preserves its exit code.

Local keyboard controls:

- `Ctrl-]` detaches even when a remote foreground program is running.
- During a path prompt, `Ctrl-C` cancels only the transfer associated with that prompt.
- With active transfers but no path prompt, `Ctrl-C` cancels the most recently added active transfer. Other transfers continue. Task IDs appear in prompts and progress messages.
- With no file transfers, `Ctrl-C` goes to the remote program. Detaching or losing the connection cancels all transfers on that connection.

File selection uses command-line path input. UTF-8 paths, spaces, surrounding quotes, and `~/` are supported. Command substitution and wildcard expansion are not performed. For downloads, press Enter to accept the default filename in the current local directory, or enter a directory or full destination path.

A session supports simultaneous uploads and downloads, with at most two active tasks, including tasks waiting for a local path. For example, start two downloads remotely:

```sh
sshore download ./a.zip &
sshore download ./b.zip &
wait
```

The client prompts for each destination in turn; confirmed transfers continue while another prompt is open. Starting a third independent active task returns a concurrency-limit error. `sshore download FILE...` instead processes the batch with two workers. Failed files do not cancel the remaining files, and the command reports an aggregate exit status. A batch uses the same two transfer slots and creates no additional SSH connections.

## Behavior and limitations

- Sessions preserve the shell, working directory, environment variables, and running programs. Terminal screens and scrollback are not saved or restored. Press `Ctrl-L` after reconnecting if a redraw is needed.
- While detached, the daemon drains and discards PTY output so programs do not block on an unread terminal. There is no automatic reconnection; run the same connection command again.
- Each named session has its own background process and PTY, with no global daemon. The session ends when its shell exits. Reboots, process termination, or system login-session cleanup policies can also end it.
- Each file task has a separate smux stream and goroutine. Transfers can run in both directions; closing one file stream does not close the terminal or other transfers. Directories, symbolic links, resumable transfers, drag and drop, nested SSH, and protocol interoperability are not supported.
- Files use 64 KiB chunks with size and SHA-256 verification. The receiver writes a temporary file, then publishes it atomically without overwriting an existing file. Normal cancellation and disconnection remove temporary files; a forcibly killed receiver or system crash may leave `.sshore-*.part` files.
- Publishing requires hard-link support on the destination filesystem, such as common Linux filesystems or Windows NTFS. Unsupported filesystems produce an error, without falling back to an operation that might overwrite a file. Permissions, timestamps, and other metadata are not copied. Received files default to `0600` on Linux and inherit the destination directory's ACL on Windows.
- These are application-level streams, not native OpenSSH channels. smux v2 provides per-stream flow control with a 256 KiB receive window per stream, a 2 MiB total receive budget, and a 16 KiB maximum frame size. File I/O runs outside the terminal event loop, and file bytes never pass through the PTY. There is no hard real-time priority guarantee; the shared TCP connection remains subject to network head-of-line blocking.
- During file prompts, keyboard input is not forwarded remotely. Up to 256 KiB of remote output is buffered; excess output is omitted with a notice. Path prompts provide basic line editing, without full shell completion.
- SSH agent and X11 forwarding environments come from the connection that created the session and are not refreshed on reconnect. Forwarding is not guaranteed to remain usable after reconnecting.
- Login scripts that print to stdout for noninteractive SSH connections interfere with the protocol handshake. Restrict such output to interactive shells.

## Runtime directory

sshore prefers `$XDG_RUNTIME_DIR/sshore`, falling back to `/tmp/sshore-UID`. The directory must belong to the current user with mode `0700`. Override it with `SSHORE_RUNTIME_DIR`, using a short absolute path under an existing parent directory. Unix socket paths have a limited length.

Each session's `.sock` is its communication endpoint; `.lock` and `.start` coordinate process lifetime and startup; `.log` records background errors. Lock files and logs may remain after a session exits and do not indicate a running session. Session listing queries active sockets only.

The internal commands `sshore bridge NAME`, `sshore daemon NAME`, and `sshore sessions` support SSH bridging and do not need to be started manually in normal use.

## Verification

```sh
go test -race ./...
go vet ./...
go build -buildvcs=false -o dist/sshore .
python3 tests/ssh_e2e.py
```

Go tests cover framing, file integrity, overwrite protection, cancellation cleanup, and real Linux session processes: name isolation, single-client attachment, window resizing, exit codes, and PID/environment preservation after detaching or disconnecting. Additional cases cover bidirectional transfers, rejection of a third task, continued upload and terminal activity while a download stream is deliberately unread, independent cancellation, and cleanup of two uploads after disconnection. Client file workers have separate concurrency tests. Integration tests build a binary with the race detector enabled.

`tests/ssh_e2e.py` requires Linux, Python 3, and OpenSSH's `ssh`, `sshd`, and `ssh-keygen`. It creates temporary keys, configuration, and an SSH server bound only to `127.0.0.1`. It exercises the real CLI, PTY, uploads, downloads, batches, download directories, overwrite protection, path prompts, terminal restoration, and file transfers after passphrase authentication without reauthentication. It does not modify the user's SSH configuration or system services.

CI runs Go tests and the race detector natively on Windows and Linux, plus real SSH/PTY end-to-end tests on Linux. Windows input uses `ReadConsole` directly, with tests for control keys such as Ctrl-Z and segmented UTF-16 decoding. Full interactive behavior in Windows Terminal still requires manual testing on a Windows machine; cross-compilation does not establish that coverage.

## Code layout

| File | Responsibility |
| --- | --- |
| `main.go` | Commands and arguments |
| `client.go`, `terminal_*.go` | SSH subprocess, terminal handling, and local interaction |
| `server_linux.go` | Session processes, PTYs, Unix sockets, and SSH bridging |
| `helper_linux.go` | Remote upload, download, and detach commands |
| `mux.go` | smux v2 configuration, version handshake, and SSH pipe adaptation |
| `protocol.go` | Message format within logical streams |
| `client_files.go` | Independent file tasks, cancellation, and progress |
| `files.go` | Chunked transfer, integrity checks, and atomic publication |

## Development and releases

Keep responsibilities small: OpenSSH handles authentication and encryption, a Linux daemon owns the PTY, smux multiplexes logical streams, and the file protocol handles regular-file transfer and integrity. There is no configuration framework, database, or additional network service.

CI checks formatting, runs `go vet`, executes race tests on both platforms and SSH integration tests on Linux, and builds four platform targets with CGO disabled. Packages contain the executable, README, and MIT license. Push a tag such as `v0.2.1` to embed the release version in the binaries. The workflow publishes archives and `SHA256SUMS` only after all tests and builds pass.

## Acknowledgments and references

Thanks to the authors and maintainers of these projects:

- [dtach](https://github.com/crigler/dtach): a reference for keeping a terminal process alive independently of its attached client, with a deliberately small detach/attach model.
- The original `dssh.nu` prototype supplied for this project: the starting point for combining SSH with persistent named sessions.
- [trzsz](https://github.com/trzsz/trzsz) and [trzsz-go](https://github.com/trzsz/trzsz-go): inspiration for initiating file transfers from a remote shell with local interaction. sshore implements its own protocol and does not use or interoperate with trzsz.
- [OpenSSH](https://www.openssh.com/): the external SSH client and server providing authentication, encryption, and connection configuration.
- [smux](https://github.com/xtaci/smux): the Go dependency providing stream multiplexing and flow control over the SSH transport.
- [creack/pty](https://github.com/creack/pty): the Go dependency providing Linux PTY creation and resizing.
- [Go](https://go.dev/), [golang.org/x/term](https://pkg.go.dev/golang.org/x/term), and [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys): the language, runtime, terminal support, and platform APIs used by sshore.

References and inspiration are distinct from runtime dependencies. Dependency versions are recorded in `go.mod`; each upstream project retains its own license.

## License

[MIT](LICENSE).
