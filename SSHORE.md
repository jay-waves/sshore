# sshore development

This is the development document. Read [AGENTS.md](AGENTS.md) before changing
code; use [GLOSSARY.md](GLOSSARY.md) for terminology and
[SECURITY.md](SECURITY.md) for security decisions. The [README](README.md) is the
short user manual. CLI help is the executable command reference.

## Architecture and code map

sshore is a Go CLI using system OpenSSH for authentication and transport. Remote
persistent shells run on Linux; local clients and resource workers support Linux
and Windows on amd64 only. Mounts and independent forwards do not share a shell
session's connection or smux streams.

| Area | Entry points and responsibility |
| --- | --- |
| CLI | `main.go`, `ssh_options.go`: routing, help, product arguments and SSH-only suffix after `--` |
| Connection and setup | `client.go`, `remote_bootstrap.go`: OpenSSH launch, remote preparation, attachment and recovery |
| Session | `server_linux.go`, `session_*.go`: daemon, PTY lifecycle, names, selection, process snapshots and termination |
| Wire protocol | `mux.go`, `protocol.go`: control, terminal and file streams over smux within SSH |
| Direct copy | `copy_command.go`: local cp parsing, batch preflight and independent SFTP transport |
| Approval and transfer | `approval*.go`, `files.go`, `transfer_sftp.go`: pending metadata, local consent, reconnect checks, file operations and progress |
| Independent resources | `resource_session.go`, `resource_status.go`, `resources.go`: worker startup, private records, control and exit receipts |
| Forward | `endpoints.go` and `forward*.go`: listener/service operands, fixed endpoint mapping and independent SSH transport |
| Mount | `ftp*.go`, `sftp_timeout.go`: SFTP adapter, FUSE/WinFsp, mount options and request deadlines |
| Inventory | `inventory*.go`, `remote_tasks.go`: public status views and advisory peer records |
| Distribution | `install.sh`, `install.ps1`, `scripts/package.py`, `scripts/release.sh`, `.github/workflows/`: installation, packaging, CI and releases |

The `ftp` source prefix is historical; the public command is `mount`, and the
protocol is SFTP. Do not infer public command names from internal filenames.
Source comments explain local invariants and algorithms; cross-component design
belongs here, not in file headers or a second documentation tree.

## Connection and session lifecycle

`new` reserves an unused name and creates a PTY session. Automatic names come
from a fixed pool; callers can supply a name when the pool is exhausted. `attach`
resolves an existing detached session and never evicts another client. Omitting
its name selects the only detached session or opens the shared picker. Selection
uses exact identities or displayed numbers, supports `/text` filtering, and does
not hold the account setup lock while waiting for terminal input.

Public names are labels backed by private metadata and lifetime locks. Rename
changes the label while preserving the daemon's socket/helper identity. Pending
approvals keep their original name and shell PID and must be requested again
after a rename. Process-tree listings are best-effort snapshots, not identities
that can be used directly for signaling.

Only explicit detach preserves the session. Exit or detected transport loss
closes the PTY and ends the daemon. This follows terminal hangup semantics; it is
not a general process containment mechanism. Detached output and terminal screens
are not replayed. On reattachment, terminal mode restoration and foreground redraw
are best effort. See [termination guarantees](SECURITY.md#files-and-forwarding).

The full release version is the compatibility identifier at setup and handshake.
There is no separate sshore protocol counter. Running daemons are replaced through
account setup, not upgraded in place. Recovery uses a transient PTY on the existing
SSH connection; see [setup and recovery safeguards](SECURITY.md#remote-setup-and-updates).

## Streams and approved operations

smux v2 carries logical control, terminal and file streams over one SSH connection;
these are not native SSH channels. The receive budget is bounded across streams,
with per-stream windows. TCP head-of-line blocking remains possible. Keepalives
detect transport loss; they do not imply permission to detach. Numeric framing
limits and buffer sizes are defined in `protocol.go` and `mux.go`.

A remote helper submits bounded operation metadata. The connected client saves a
local pending record and detaches. A separate local `review` invocation selects
the record, collects consent, consumes it, reconnects and checks the original
session before executing. Setup updates or recovery skip approved operations.
The authorization rules belong in [Local approval](SECURITY.md#local-approval).

File batches run sequentially and stop at the first failure. Both direct and
approved copies use the same SFTP upload/download implementation in `files.go`.
Uploads create private temporary files, verify stored bytes by SHA-256 readback,
and publish with a no-overwrite hard link. Downloads check the exact source size
and sync a local temporary file before publication. Authenticated SSH provides
transport integrity; neither mechanism authenticates file provenance.

Direct cp opens a system OpenSSH SFTP subsystem without installing sshore,
creating a session or mounting a filesystem. It preflights the whole batch. A bare
`-` selects the invoking process's stdin/stdout for one direct remote copy.
Stdin is streamed into the remote temporary file while counting and hashing
bytes; EOF determines the size used for readback verification and publication.
It is not spooled locally. Stdout writes directly and may contain partial data
on failure. Streams cannot participate in remote approval requests because
those detach and resume in another process.
Approved copies instead open a client-initiated smux stream on the revalidated
session. A bounded metadata handshake selects one file, then switches to SFTP.
The embedded request handler exposes only virtual `/source` or `/target` and
one upload temporary file; it cannot list directories or access other paths.
Before linking an approved upload, the server checks its size and syncs it.
Stream termination closes handles and removes its temporary file.

Progress reports batch position, smoothed speed and ETA. Completed files remain
when later operations fail. Cancellation closes only the transfer stream (or
the direct copy's SSH pipes); cleanup is best effort after process termination.
The client releases terminal input before SSH authentication and retains the
existing resume/`--stay` behavior after approved copies.

## Independent workers

A local mount or forward starts a worker that owns its transport and resource.
The launcher handles authentication through a temporary askpass broker until
startup acknowledgement. Losing the launcher before acknowledgement cancels
startup. Per-name locks prevent duplicate workers; authenticated loopback control
performs status and cancellation without signaling a PID read from disk.
Resource records, peer snapshots, session names and exit receipts share private
temporary-file writes: sync and close before replacing metadata. File transfers
retain no-overwrite publication; approvals retain exclusive creation and consumption.

The public transfer commands are `cp SOURCE... TARGET` and
`forward LISTENER SERVICE`. `origin:` and `:` denote the client's endpoint;
`origin:PATH` and `origin:PORT` can fill it explicitly. Local direct commands use
`HOST:PATH` or `HOST:PORT` for the remote side. Remote helpers use the current
server context and never initiate another SSH connection.

Forward requests always store the listener in `port` and service in `target_port`.
Reverse means the server listens. The client port stays zero until review, even
when `local_hint` suggests a port; a suggestion never authorizes execution.
Review fills that missing port and clears the hint.
Unresolved requests cannot execute. File requests likewise retain an optional
local path hint; review validates the selected path and clears the hint before
execution. Relative local suggestions are resolved in the review process's
working directory, with home expansion supported. Internal upload/download
operation kinds and forward/NAME resource IDs remain; the public upload/download
and publish/subscribe commands are removed.

Submitting all validated local paths or a valid local port grants consent;
there is no separate yes prompt. Enter accepts a displayed suggestion.
An empty port without a suggestion retries. File batches collect every path
before any transfer starts and reject duplicate destinations. Cancellation
during collection leaves the request pending.
After validation the source shell is explicitly detached and the worker
runs independently. Local forwards may opt into at most ten retries after an
established connection fails. Retry backoff is bounded; authentication is
noninteractive, host keys must already be trusted, and TCP connections are not
resumed. Cancellation also interrupts backoff. Initial startup failures are not
retried. Native SSH forwarding remains owned by its SSH connection.

Mounts use a separate SFTP subsystem without requiring remote sshore installation.
Linux uses FUSE; Windows uses WinFsp. Writable mounts require atomic-rename and
fsync extensions. There is no persistent write-back cache, conflict resolution
or replay. Linux read caching lasts until close/reopen unless direct I/O is used;
Windows rejects unrepresentable names instead of renaming them. Special files and
file locking are unsupported.

Each outstanding SFTP request has its own deadline; sibling responses do not
extend it. Timeout closes the transport and fails pending operations without
claiming that remote writes were cancelled. Startup timeout is separately opt-in
and includes authentication. Idle mounts have no request timer. Unmount normally
runs while the transport is live; busy mounts stay available for retry, while a
second interrupt or shutdown timeout forces disconnection. See
[SFTP mount safeguards and limits](SECURITY.md#sftp-mounts).

## Inventory and diagnostics

Local status separates worker liveness, transport state and unprobed target health.
Resource queries use at most eight workers and a two-second aggregate deadline.
`list --details` and `list --json` include recent resource results; JSON uses an
explicit field allowlist. Remote detailed inventory requires a matching release
and never triggers installation. CLI help defines the output contract and errors.

Peer records are advisory mirrors, not resources or permissions. Mounts publish
through SFTP; forwards use a same-release watcher when available and otherwise
keep the primary forwarding transport working. Sync failure does not fail the
resource. Records live in the remote account's `.local/state/sshore/peer-tasks`;
a custom SFTP root may produce an inventory not visible to the shell. Refresh is
about every 20 seconds; after 75 seconds a missing heartbeat becomes `unknown`.
Pending mirrors expire after ten minutes, other stale mirrors after 24 hours.

Local resource records/logs and remote session runtime state are separate. Use
`list --details` for resource log locations. Remote session logs live under
`$XDG_RUNTIME_DIR/sshore` or `/tmp/sshore-UID`, with `SSHORE_RUNTIME_DIR` overriding
the runtime directory. Session exit records and local resource receipts are bounded
to 100 per category for seven days. They do not contain terminal replay or prove
that every task ended; forced termination and runtime cleanup can leave no record.

## Build and validate

Use the normal shared toolchain required by `go.mod`; check `command -v go` and
`go version` first. Do not lower requirements or create a private toolchain to
make checks pass. Build a development binary with:

```sh
go build -o dist/sshore .
```

For routine changes, run the relevant tests with `-run`; use the core suite
when changes span components:

```sh
go test -tags=sshoretest -timeout=5m ./...
```

Use `go vet ./...` for structural changes and add `-race` for concurrency changes.
Run installer checks only when installation or packaging changes. Real SSH
lifecycle tests are needed for transport/setup changes and must use an isolated
tagged executable:

```sh
go build -tags=sshoretest -o dist/sshore-e2e .
python3 tests/ssh_e2e.py --binary dist/sshore-e2e
```

Never run simulated upgrade confirmation against the developer's real sessions.
Release binaries must not use `sshoretest`. Native mount tests are opt-in and
require the platform mount environment; cross-compilation does not validate it.
The manually triggered Tests workflow runs core Go checks on Linux/Windows.
Its `extended` option adds race detection, installer checks and Linux SSH
integration. Build validation remains in release CI; do not repeat full checks
after a successful run unless further changes warrant it.

`experiments/vt10x` is an isolated module probing a possible terminal observer.
Its compatibility failures represent unmet requirements, not product regressions;
it is excluded from the root module and release builds. The local SFTP concurrency
benchmark in `sftp_benchmark_test.go` is an in-process baseline, not WAN performance
evidence. Do not tune production defaults from that result alone.

For documentation changes, check relative links, Markdown fences and affected
Mermaid diagrams. README depicts user flow; SECURITY depicts trust boundaries.
Keep numeric defaults in CLI help and their implementation unless a design or
security explanation needs them.

## Release

`VERSION` and the source default in `main.go` must agree and use a full semantic
version. Put version-specific changes and migration notes in `scripts/release.sh`.
Do not accumulate a changelog in the project documents.

CI builds Linux and Windows amd64 on pushes and pull requests. On `main`, after
both builds succeed, the release job checks that the commit is still the branch
head, creates the new tag, and publishes archives plus `SHA256SUMS`. Builds use
`-trimpath` and version linker flags without `sshoretest`. Existing tags, releases
and assets are immutable; bump the version to publish different content. Full
tests are run separately and are not an automatic release gate.

Archives contain the executable, project documents and license notices. Verify
release status, commit/tag identity and both platform assets after publishing.
A source-only change at an already published version does not replace that release.
