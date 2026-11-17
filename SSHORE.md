# sshore development

Read [AGENTS.md](AGENTS.md) for contributor rules, [GLOSSARY.md](GLOSSARY.md)
for terminology and [SECURITY.md](SECURITY.md) for security guarantees.
[README.md](README.md) is the user manual; CLI help owns syntax and defaults.

Code and tests own implementation details; this guide covers ownership, design
constraints and development workflows.

## Architecture and code map

sshore is a Go CLI using system OpenSSH only to evaluate configuration (`ssh -G`).
A system SSH agent or interactive credentials authenticate; Go owns the connections.
The root program coordinates commands, release policy and cross-component
lifecycles. Internal packages provide mechanisms without owning application
policy or depending on mutable application state.

| Responsibility | Start here |
| --- | --- |
| MCP tools and remote Bash | `internal/mcp/`, `internal/tmux/bash_linux.go`; application coordination in `mcp_client.go`, `mcp_server_linux.go` and the shared daemon |
| One-off SSH execution | `cli.go`: exec adapter uses the shared SSH connection layer without remote setup |
| CLI grammar, dispatch and help | `cli_schema.go`: shared command aliases and product/SSH grammar; `cli_commands.go`: command semantics; `cli.go`: dispatch; `cli_tasks.go`: short-ID selection |
| Connections and session switching | `client.go`, `client_connection.go` |
| Remote preparation and exact-release policy | `setup*.go` |
| Shell hosting, identity and cancellation | `launch_*_linux.go`, `session_linux.go`, `server_linux.go`; process inspection in `setup_process_linux.go` |
| Local daemon and shell registry | `local_daemon*.go`, `local_shell.go`, `platform_*.go` |
| Spawn ownership, lifetime SSH and nested routes | `spawn_transport.go`, `spawn_route_linux.go`, `platform_*.go` |
| Shell startup and attachment I/O | `internal/tmux/terminal.go`, `internal/tmux/shell_attachment.go`, application adapters in `protocol.go` |
| Native terminals, console I/O and shortcuts | `internal/tmux/` |
| Local prompts and presentation | `internal/termui/`, application adapters in `cli_ui.go` |
| Framing, smux and shared SSH transport | `internal/ssh/` (configuration, public-key authentication, host verification, connections, forwarding and framing); application messages and mux handshake in `protocol.go` |
| Copy and mount mechanisms | Flat `internal/files/`; command adapters in `cp_command.go`, `mount*.go` |
| Review records and consent | `review_store.go`, application coordination in `review.go` (including shared source-shell validation in `review_linux.go`) |
| Forward transport and retries | `forward.go`: endpoint parsing, command execution and SSH worker lifecycle |
| Resource startup and control | `resource_runtime.go`, `resources.go` |
| Private storage and name reservations | `state.go` (including account identity and remembered entry points), `task_identity.go`; native checks in `platform_*.go` |
| Inventory, stable IDs and shell relationships | `inventory*.go`, `list*.go`, `shell_cache.go`, `pstree*.go`, `peers*.go` |
| Platform adapters | `platform_linux.go` / `platform_windows.go` in the application and file/transport packages; `console_linux.go` / `console_windows.go` in terminal/UI packages |
| Product workflow tests | `tests/`: shared isolated CLI build, public command workflows and native terminal handoff; module and private protocol regressions remain with their owning code |
| Installation, packaging and publication | `install.sh`, `install.ps1`, `scripts/`, `.github/workflows/` |

Keep command policy in the application: authorization, release checks, prompts,
identity validation and resource coordination. Pass validated configuration and
small interfaces to mechanisms. Extend the owning module before adding an
abstraction; move behavior with its tests and platform implementations.

## Design constraints

### CLI parsing and help

`cli_schema.go` owns command grammar; command adapters interpret parsed operands.
`cli_help.go` owns command help and concise workflow examples (`--tldr`).
See [CLI ownership](AGENTS.md#cli-ownership).

### Terminal and mux design scope

Support Linux and Windows **amd64 only**, with remote shell hosting on Linux.
See the [OS and shell support matrix](README.md#os-and-shell-support) for
supported systems and shells. Windows provides local ConPTY sessions and SSH
clients, not a remote shell server.
ConPTY children join their kill-on-close Job during process creation. Native
console I/O remains synchronous as required by ConPTY; pinned I/O threads use
CancelSynchronousIo before transferring input ownership.
Each interactive shell owns one terminal and permits one attachment at a time. MCP
Bash sessions use pipes and do not support terminal attachment. Split panes,
tiling, screen emulation and scrollback replay are outside scope.

The shell and native terminal retain interactive state. Redraw is best effort;
do not inject Enter, Ctrl+L or editor commands to reconstruct it. Terminal bytes
must retain order and bounded buffering. Unsupported terminal protocols must not
silently become local shortcuts.

### SSH connections

`internal/ssh` owns one connection and any jump chain per task; closing an
attachment does not close a daemon-owned spawn lease. OpenSSH configuration is
resolved in the initiating environment and directory, including its remote
command. No persistent SSH subprocess,
ControlMaster, askpass executable or implicit reconnect is used.

MCP reuses one background SSH transport per host. Successful calls leave it open;
cancelling a call closes its transport, while submitted scripts may continue.
Failed requests are never replayed automatically; the next call reconnects.
Closing MCP releases transports without ending persistent remote Bash sessions.

The server may accept SSH `none` authentication (for example Tailscale SSH).
Only when public-key authentication is requested is the system agent queried. Linux
uses a Unix agent socket; Windows uses its OpenSSH agent pipe. Configuration
resolution and agent connections are per connection. Unsupported execution hooks and authentication
modes fail explicitly. Keep resolver arguments compatible with Windows in-box
OpenSSH; Windows system path tokens use the resolver's initiating environment.
Modern algorithm negotiation uses the supported Go SSH
algorithms intersected with resolved configuration, preserving configured order
within the allowed set. SSH debug logging (`-v` or `LogLevel DEBUG`) reports the
negotiated key exchange, host-key algorithm and both traffic directions, marking
AEAD integrity protection. Normal connections remain quiet. Host verification uses the
local known_hosts policy; unknown-host confirmation is routed to the initiating
frontend over private local control. Background retries never prompt.

After managed shell preparation, the client opens an SSH stream-local channel to
the account's private daemon socket. The preparation process exits; no bridge
helper remains per connection. This requires the SSH server to permit stream-local
forwarding. Recovery retains its transient exec channel instead.

Native port forwards belong to their connection. Application forwards retain
separate connections and their explicit retry policy. Cancellation closes owned
connections to interrupt channel I/O before joining workers. Preserve this order
when adapting streams; session stdin must serialize EOF with writes.

### Shell ownership and attachment

Keep lifetime separate from interaction:

- `spawn` belongs to its original parent process; remote spawn also requires its
  lifetime SSH connection. Detach leaves that connection active.
- `launch` is independent of the launching client. Detach, switching and
  reattachment never change a shell's startup lifetime or transfer its owner.
- Attachment is exclusive. A stale attachment must not release its successor.
  Failed creation may roll back its new terminal; failed reattachment must
  preserve the existing shell.
- Cancel and join old input readers/writers before handing the terminal to
  another attachment, local prompt or SSH authentication process.

The client at the user's terminal owns terminal input and shortcuts. Interactive nested `spawn` commands ask
the existing daemon and client to prepare a child attachment, then exit only
after the child is ready. Preserve the original owner PID and creation identity;
the short-lived launcher and terminal client never become its lifetime owner.
Drain parent input before switching. Detach and child exit unwind one level;
failed startup restores the parent and reports failure to its launcher.
Interrupts during authentication or setup must cancel the child connection and
restore the parent; signal handling stays active through attachment transitions.
Keep the frontend's authenticated transport alive across nested attachments,
route confirmations and returns to the parent. Closing an attachment releases
its stream; closing the frontend releases its retained transports. Saved routes
contain no live connection handles and still require authentication.

For a next-hop connection, the initiating host's daemon uses that shell's
configuration, signing credentials, environment and working directory. The terminal client follows
a private attachment route, not remote-supplied local execution instructions.
Local managed shells also support handoff for remote `launch` and `attach`.
Detached startup keeps its noninteractive path. Reuse the existing connection and
attachment mechanisms; do not run a second terminal frontend inside a daemon.

Local handoff lives in `client_connection.go`; remote child ownership and routing in
`spawn_route_linux.go`; the frontend return stack and connection restoration in
`client_connection.go`. Reattachment and review must preserve the route and its
parent instance. If it expires or changes, fail rather than reinterpret the
child's host alias as a direct destination. See the
[handoff trust boundary](SECURITY.md#trust-boundaries).

Commands select tasks by name or short ID. The task index reserves short IDs against
immutable instances and preserves internal ordering. Short-ID operations reuse the
connection settings of their observation without changing the recent-attach default.
Actions revalidate immutable
instances, capabilities or process identities at their authority. Displayed process
ancestry is not permission to signal. Follow the
[shell control safeguards](SECURITY.md#shell-control-and-lifetime).

### Shared local daemon and resource workers

A daemon owns local shell terminals and forward SSH connections. Mounts use separate
workers to isolate native filesystem work. Remote shells are daemon-owned; mounts
and independent forwards retain their own SSH transports and lifetimes.

Startup must have an explicit ownership handoff. Carry the initiating client's
working directory and environment, not whichever environment started the daemon.
Do not hold bookkeeping locks across terminal I/O, authentication or worker startup.
Daemon liveness comes from its owner lock; orphaned metadata cannot block startup
with a version mismatch. `cancel --all` asks the existing daemon to exit as soon
as its remaining workers finish, without the normal idle grace period.
Cancellation must close owned resources and join background work before reporting
completion; persisted PIDs are diagnostic, not authority to terminate.

Keep permission and transport separate. Direct commands are local grants; remote
requests require separate local review before execution. Copy preflight creates
nothing, approval consumption is single-use, and file publication must not overwrite.
State-file replacement cannot substitute for those command-specific guarantees.
See [approval safeguards](SECURITY.md#local-approval).

Copy and mount helpers use the shared preparation path before starting.
Ordinary forwarding, inventory and cancellation do not install remote software.
Keep file/transport behavior in `internal/files` and `internal/ssh`; account setup
and consent stay in the application.

Copy concurrency must stay bounded and preserve exact-size checks, ordered digest
calculation, receiver-side upload verification and no-overwrite publication.
Transport window budgets live in `internal/ssh/protocol.go` and `internal/files/mount.go`; size them
for latency without allowing stalled file streams to exhaust terminal capacity.

Mounts always use the remote sshore helper and the same setup/version safeguards as
`cp`; there is no plain SFTP compatibility path.

Mount failures can leave writes uncertain; never replay writes implicitly.
Keep caches and notifications bounded and advisory. Invalidate affected paths
and parents on mutation; notification loss or unknown scope requires full
invalidation. Background verification must not delay foreground reads.
Scheduling and cache behavior belong in `internal/files/` and its tests.

Windows mount metadata-only opens reuse bounded, short-lived SFTP handles in
`internal/files/mount_metadata_windows.go`. Data and mutation opens remain separate;
active metadata leases keep their original remote handle after cache retirement.
Handle retention, attribute freshness and capacity probes have separate lifetimes.
Handle reuse is bounded from opening, rather than extended indefinitely by access.
Local mutations and remote invalidation stop reuse; expiry and unmount close idle
handles. Volume capacity, including probe failures, uses the metadata cache and is
invalidated by mutations. These caches do not buffer file writes.

### Inventory and diagnostics

Separate live observations, cached records and unknown state. Inventory and
`pstree` relationships never authorize operations. Task IDs are allocated at creation;
alias changes preserve the instance and storage identity. Prefix collisions at creation
require a new identity; imported ambiguous references require an explicit scope.
Keep allocation and reference policy in `task_identity.go`, separate from capabilities.
Spawn owner context stays fixed;
the current attachment source may change. Neither is verified OS process ancestry.

Keep normal output compact and distinguish cached, live and unknown state.
Full diagnostics belong in `list --json`. Incoming resources require a live
local transport registration, not merely advisory peer files. Worker liveness
is not target health. Exclude capabilities and credentials from public output.
See [inventory privacy](SECURITY.md#inventory-and-privacy).

### Setup and recovery

Require the exact full release version; do not add a separate compatibility counter.
Never silently downgrade or treat unordered versions as safe to replace. Preparation
must succeed before confirmed tasks are ended, with identity/snapshot rechecks under
the account setup lock. Keep these checks with their lifecycle callers when refactoring.

Recovery retains the authenticated SSH connection but disables normal sshore
operations until reconnecting. It is a product restriction, not a sandbox.
[SECURITY.md](SECURITY.md#remote-setup-and-updates) owns the complete update,
cancellation and recovery guarantees.

## Build and validate

Use the shared toolchain required by `go.mod`; check its location and version
before development. Do not lower requirements or introduce a private toolchain.

```sh
command -v go
go version  # Go 1.26 or newer
go build -o dist/sshore .
```

Choose the smallest check that covers the changed behavior:

| Change | Validation |
| --- | --- |
| One module or command | Relevant existing tests with `-run` |
| Cross-component behavior | Core suite below |
| Structure or concurrency | `go vet ./...`; add `-race` for concurrency |
| SSH transport or remote setup | Isolated real-SSH lifecycle checks |
| Native terminal or mount behavior | Relevant native platform checks; compilation alone is insufficient |
| Installation or packaging | Relevant scripts in `tests/` and release checks |
| Documentation | Relative links/anchors, code fences and affected Mermaid diagrams, including packaged documents |

```sh
go test -count=1 -tags=sshoretest -timeout=5m ./...
```

Group tests by behavior and preserve platform/build-tag isolation.
Product workflows in `tests/` use its shared isolated CLI build; module regressions
stay beside their owning code. Do not expose internals or use source overlays.
Subprocess builds inherit `GOFLAGS`; use `GOFLAGS=-race` for race checks and
`-count=1` for environment-dependent checks.

Lifecycle integration executables **must** use `sshoretest` for runtime-directory
isolation. Never simulate upgrade confirmation against the developer's real shells.
Release binaries must not use this tag. For real-SSH checks:

```sh
go build -tags=sshoretest -o dist/sshore-e2e .
python3 tests/ssh_e2e.py --binary dist/sshore-e2e
```

Native mount smoke tests are opt-in and require an existing FUSE/WinFsp environment:

```sh
SSHORE_TEST_FTP_MOUNT=1 go test -count=1 -tags=sshoretest -run '^TestFTPNativeMount$' ./internal/files
```

The manually triggered Tests workflow defines Linux/Windows checks and optional
extended validation. Do not repeat successful checks without new changes or
unresolved concerns. Local in-process benchmarks are not WAN performance evidence.

### Shell and terminal validation

Supported targets are listed in the [README matrix](README.md#os-and-shell-support).
PowerShell interactive validation uses PSReadLine.
Existing tests are evidence for their covered behavior, not a fully validated
terminal/shell matrix. User reports and cross-compilation do not replace native
interactive checks.

For terminal changes, check pending input/cursor retention, redraw, interrupt and
job control across detach/reattach, fragmented escape sequences, paste boundaries
and cleanup before returning to local prompts. Preserve platform/build-tag
isolation. Enhanced keyboard protocols, custom prompts and terminal capabilities
can affect behavior; record meaningful regressions as focused tests.

## Release

1. Keep `VERSION` and the source default in `main.go` equal to the new full release
   version. Add changes and migration consequences in `scripts/release.sh`.
2. Run checks appropriate to the changes, then commit and push. A push to `main`
   triggers Linux/Windows amd64 builds and packaging; successful builds permit
   publication only while that commit remains the branch head.
3. Unless explicitly asked only to push, verify the release, commit/tag identity,
   both platform archives and checksums. Build CI does not run the full core suite;
   the separate Tests workflow does.

Published tags and assets are immutable. Source-only edits at an already published
version do not update its archives. Retain only the highest published patch in each
major/minor series after verifying complete replacement assets; never delete Git
tags or prune on failed/incomplete publication. Drafts, prereleases and noncanonical
tags are excluded. Packaging and retention rules live in `scripts/package.py`,
`scripts/release.sh` and `scripts/prune_releases.py`.

## Planned work

`pull` / `push` directory synchronization is not implemented. The current proposal
is one-shot by default, optional background watching, explicit opt-in to propagated
deletions, and conflict reporting for independent edits. Revisit the design before
implementation; it is not an existing command contract.

`cli.go` orchestrates reset across private state stores; `state.go` owns account display identity and remembered connection entry points without depending on CLI options. Native OS identity reads live in the platform adapters. Reset preserves active ownership, lock files, approvals and non-recycled task IDs.
