# sshore development

Read [AGENTS.md](AGENTS.md) for contributor rules, [GLOSSARY.md](GLOSSARY.md)
for terminology and [SECURITY.md](SECURITY.md) for security guarantees.
[README.md](README.md) is the user manual; CLI help owns syntax and defaults.

This document is a map and a set of design constraints. Code and focused tests
own algorithms, record formats, protocol fields, limits and platform behavior.
Update this document when ownership, scope or the development workflow changes;
do not mirror implementation details here.

## Architecture and code map

sshore is a Go CLI using system OpenSSH only to evaluate configuration (`ssh -G`).
An SSH agent signs authentication requests; Go owns the network connections.
The root program coordinates commands, release policy and cross-component
lifecycles. Internal packages provide mechanisms without owning application
policy or depending on mutable application state.

| Responsibility | Start here |
| --- | --- |
| MCP tools and remote Bash | `internal/mcp/`, `internal/tmux/bash_linux.go`; application coordination in `mcp_client.go`, `mcp_bridge_linux.go`, `mcp_registry_linux.go` and the shared daemon |
| One-off SSH execution | `cli.go`: exec adapter uses the shared SSH connection layer without remote setup |
| CLI grammar, dispatch and help | `cli_schema.go`: shared command aliases and product/SSH grammar; `cli_shell.go`, `cli_endpoints.go`: command semantics; `cli.go`: dispatch; `cli_tasks.go`: short-ID selection |
| Shell completion | `completion.go`: contexts, candidate protocol and read-only hints; `completion_scripts.go`: shell adapters |
| Connections and session switching | `client.go`, `client_connection.go`, `client_handoff.go` |
| Remote preparation and exact-release policy | `setup*.go` |
| Shell hosting, identity and cancellation | `launch_*_linux.go`, `session_*.go`, `server_linux.go`; process inspection in `setup_process_linux.go` |
| Local daemon and shell registry | `local_daemon*.go`, `local_shell_registry.go`, `local_terminal.go`, `platform_*.go` |
| Spawn ownership, lifetime SSH and nested routes | `spawn_transport.go`, `spawn_route_linux.go`, `platform_*.go` |
| Shell startup and attachment I/O | `internal/tmux/terminal.go`, `internal/tmux/shell_attachment.go`, application adapters in `protocol.go` |
| Native terminals, console I/O and shortcuts | `internal/tmux/` |
| Local prompts and presentation | `internal/termui/`, application adapters in `cli_ui.go` |
| Framing, smux and shared SSH transport | `internal/ssh/` (configuration, agent authentication, host verification, connections, forwarding and framing); application messages and mux handshake in `protocol.go` |
| Copy and mount mechanisms | Flat `internal/files/`; command adapters in `cp_command.go`, `mount*.go` |
| Review records and consent | `review_store.go`, application coordination in `review*.go` (including shared source-shell validation in `review_linux.go`) |
| Forward transport and retries | `forward.go`: endpoint parsing, command execution and SSH worker lifecycle |
| Resource startup and control | `resource_runtime.go`, `resources.go` |
| Private storage and name reservations | `state.go` (including account identity and remembered entry points), `object_names.go`; native checks in `platform_*.go` |
| Inventory, stable IDs and shell relationships | `inventory*.go`, `list*.go`, `shell_cache.go`, `pstree*.go`, `peers*.go` |
| Platform adapters | `platform_linux.go` / `platform_windows.go` in the application and file/transport packages; `console_linux.go` / `console_windows.go` in terminal/UI packages |
| Installation, packaging and publication | `install.sh`, `install.ps1`, `scripts/`, `.github/workflows/` |

Keep command policy in the application: authorization, release checks, prompts,
identity validation and resource coordination. Pass validated configuration and
small interfaces to mechanisms. Extend the owning module before adding an
abstraction; move behavior with its tests and platform implementations.

## Design constraints

### CLI parsing and completion

Execution and completion share the grammar and scanners in `cli_schema.go`;
command adapters interpret the parsed operands. Partial input uses the same
validation rules without requiring a complete command. Change grammar/context
tests together when extending the CLI; see [CLI ownership](AGENTS.md#cli-ownership).

Completion uses native local file completion and bounded cached hints. For its
trust boundary and execution guarantees, see [inventory and privacy](SECURITY.md#inventory-and-privacy).

### Terminal and mux design scope

Support Linux and Windows **amd64 only**, with remote shell hosting on Linux.
Windows provides local ConPTY sessions and SSH clients, not a remote shell server.
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
command. Completion never runs this resolver. No persistent SSH subprocess,
ControlMaster, askpass executable or implicit reconnect is used.

Agent-backed public-key authentication is required. Linux uses a Unix agent
socket; Windows uses its OpenSSH agent pipe. Configuration resolution and agent
connections are per connection. Unsupported execution hooks and authentication
modes fail explicitly. Modern algorithm negotiation uses the supported Go SSH
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

A root frontend owns the real terminal. Interactive nested `spawn` commands ask
the existing daemon and frontend to prepare a child attachment, then exit only
after the child is ready. Preserve the original owner PID and creation identity;
the short-lived launcher and root frontend never become its lifetime owner.
Drain parent input before switching. Detach and child exit unwind one level;
failed startup restores the parent and reports failure to its launcher.

For a next-hop connection, the initiating host's daemon uses that shell's
configuration, agent, environment and working directory. The root frontend follows
a private attachment route, not remote-supplied local execution instructions.
Local managed shells also support handoff for remote `launch` and `attach`.
Detached startup keeps its noninteractive path. Reuse the existing connection and
attachment mechanisms; do not run a second terminal frontend inside a daemon.

Local handoff lives in `client_handoff.go`; remote child ownership and routing in
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

Mount request failures can leave write outcomes uncertain: never replay writes
implicitly. Caches and notifications are advisory, bounded and invalidated by
mutations at the affected paths and parent directories, including both trees
for renames. Unrelated cache entries remain usable during local writes; unknown
scopes and notification loss retain full invalidation. Background metadata
verification prioritizes recent accesses, coalesces repeated work, debounces
bursts with a bounded delay and rate-limits verification starts. Transport failures
back off background work; foreground reads never wait for verification or its
timers. Queue bounds and scheduling policy live in `internal/files/mount_refresh.go`. Do not add synchronous alias discovery
to the mutation path. For changes, start with the SFTP, metadata-cache, refresh, prefetch and platform
adapter code and tests in `internal/files/`, rather than a second specification.

### Inventory and diagnostics

Separate live observations, cached records and unknown state. Inventory and
`pstree` relationships never authorize operations. Task IDs are allocated at creation;
alias changes preserve the instance and storage identity. Prefix collisions at creation
require a new identity; imported ambiguous references require an explicit scope.
Keep allocation and reference policy in `list_ids.go`, separate from capabilities.
Spawn owner context stays fixed;
the current attachment source may change. Neither is verified OS process ancestry.

Keep the default task list to one row per task with details bounded by the output
terminal width; redirected output preserves complete details. Do not redraw on resize;
full diagnostics and recent history belong in `list --json`. The table shows
ID with an optional alias and a separate TYPE, grouped by host. `pstree` is local:
keep the current tree first, sort remaining nodes by observation recency, omit
confirmed ended entries, and retain uncertain local records with a compact marker.
Show remote attachment sources and resource provider-to-consumer directions without
state or observation columns. Incoming resources require a live local transport
registration in `peers.go`; advisory peer files alone are insufficient. A held local shell lifetime lock remains a
local node when its status endpoint is temporarily unavailable.
Status should distinguish worker liveness from transport state and target health.
Public output must exclude capabilities and credentials; keep diagnostics out of
normal command flow. Receipt retention and peer expiry belong in their implementation
and tests. See [inventory privacy](SECURITY.md#inventory-and-privacy).

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

Group tests by behavior, such as CLI grammar, approval flows, or shell and
resource lifecycle. A regression belongs with its feature’s existing cases;
keep platform and build-tag variants separate where required. Prefer
`feature_test.go` or `feature_linux_test.go` / `feature_windows_test.go`.
The feature or its directory identifies the test scope without an extra suffix.

Product black-box tests live in `tests/` and follow user workflows through
the CLI, protocols or public APIs. Module regressions stay beside the code they
exercise, including those that use an internal package’s exported API. Both use
standard Go test discovery and `-run`;
no overlay, generated source tree or test-only production exports are needed.
Use `-count=1` for CLI/native checks that depend on subprocesses or the environment.
For example:

```sh
go test -count=1 -tags=sshoretest -run '^TestCancelAll' ./tests
```

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

Targets are bash, sh, zsh and fish on Linux; pwsh with PSReadLine and cmd on Windows.
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
