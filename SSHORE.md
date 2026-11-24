# sshore development

## Architecture and code map

sshore resolves client configuration and establishes SSH connections in Go.
`internal/sshconfig` evaluates rules without opening connections. It applies
command-line options before user and system files, retains earlier values on
the final pass, and preserves file lists as separate entries.
`internal/ssh` selects credentials and applies host-key and algorithm policy to
the Go SSH library. The library handles the wire protocol, key exchange, signing
and channels. The root package owns command policy, authorization and task
lifecycles. For exact behavior, start at the command entry point and follow it
to the platform implementation and its tests.

| Question | Start here |
| --- | --- |
| How is a command parsed and dispatched? | [Command definitions and help](cli_help.go), [parser](cli_schema.go), [dispatch](cli.go) |
| How does a client connect and authenticate? | [Client connections](client_connection.go), [configuration evaluation](internal/sshconfig/), [SSH transport and host verification](internal/ssh/) |
| How are installation and approval enforced? | [Setup](setup.go), [approval handling](review.go), [request validation](review_store.go) |
| How do copy, mount and forwarding reach their target? | [Resource commands](cli.go), [shell routes](resource_route.go), [filesystem operations](internal/files/), [forwarding](forward.go) |
| Where is behavior tested? | [External integration tests](tests/integration/) run the built CLI against OpenSSH, and installer checks run the platform scripts |

## Documentation ownership

The [README](README.md) covers user workflows, [SECURITY](SECURITY.md) defines
trust boundaries and safeguards, the [glossary](GLOSSARY.md) defines terms,
and [AGENTS.md](AGENTS.md) sets contributor constraints. Keep development and
release guidance here. CLI help owns syntax and defaults. When changing behavior,
update the responsible document and any affected diagrams.

## Terminal and mux design scope

Clients support Linux and Windows amd64. Linux amd64 is the supported controlled
host. Each interactive shell owns one terminal with one attachment at a time.
Split panes, tiling, screen emulation and scrollback replay are outside scope.
The shell and terminal preserve interactive state across detach. Redraw is best
effort and must preserve user input. See the [support matrix](README.md#os-and-shell-support)
and [native terminal adapters](internal/pty/).

Local and hosted shells share one [shell task lifecycle](shell_terminal.go).
Their entry points provide the commands specific to
local handoff or hosted resources.

Build the experimental Apple Silicon client on Apple Silicon with
`CGO_ENABLED=0 go run ./scripts go build -o sshore .`. It targets
SSH connections to Linux hosts and supports local interactive shells. Install
the client manually. Local SFTP mounts use the traditional macFUSE kernel
extension through go-fuse. Install macFUSE and enable its kernel extension before
mounting. The Apple Silicon client is built manually from source.

TODO: Add a macFUSE FSKit backend that preserves `CGO_ENABLED=0`.

Cross-compilation alone does not validate macOS terminal input, SSH
authentication, daemon lifetime or native mounts.

### Shell ownership and attachment

The account daemon owns the parent edges between its shells. It admits one
foreground terminal per tree. From outside, a frontend enters an idle root.
From a suspended parent it can enter a direct child, or adopt an idle root
and enter it in the same operation. A failed attach rolls back a provisional
adoption. A private reservation keeps the parent path occupied until the
frontend returns or disconnects. Input elsewhere in the same tree is refused.

A root follows its starting connection. A same-host child follows its parent.
For a cross-host child the initiating daemon holds an authenticated SSH route
scoped to the parent. Re-entering that child uses the same route. Each daemon
makes its own attachment decisions. Explicit detach records a pin before
releasing input and keeps the existing parent edge. When the owner ends, a
pinned child survives as a root with its own descendants. An internal handoff
preserves the original scope and pin. See [tree admission](shell_tree_control.go),
[retention](shell_retention.go) and [attachments](shell_attachment.go).

The authenticated exit frame carries the exit code and terminal output offset.
The frontend drains the reported tail within a time budget and reports the
exit even if the tail is incomplete. A remote root sends a bounded shell tree
with its final frame. The root prompt can display it immediately and reuses one
SSH connection for subsequent inventory requests.
A nested exit opens the prompt when the daemon confirms surviving pinned
children. Dismissing that prompt returns to the previous shell. A connection
loss without an exit frame leaves the state unconfirmed.

### Account daemon and mounts

One authenticated daemon per OS account owns registered shells and resources.
Normal connections require the exact full release version. Validate process
identity before signaling and preserve approval checks for shell-routed resources.
Mount and forward configuration travels in the authenticated launch request.
The upgraded stream carries readiness and the caller's commit. The mount worker
uses a separate pipe exchange because it can outlive the launch connection.
Copy clients and mount clients use `pkg/sftp/v2` alongside the embedded agents.
The pinned upstream revision includes the client disconnect fix and supports the
project's Go version. Directory change notifications use their own stream.
Trace changes through the [task registry](daemon_tasks.go), [mount coordinator](mount.go)
and [route authorization](resource_route_linux.go). Consult [SECURITY.md](SECURITY.md)
before changing update, reset or approval behavior.

### Inventory and diagnostics

Both inventory views read the current account daemon. `pstree` (`ps`) reads
only shells and displays names, short IDs, hosts and states. Its branches follow
parent UIDs and siblings sort by task ID. A leading `*` marks the current shell.
`active` means a shell holds input, `waiting` means the frontend reserved its
place while entering a child or handling an approval, and `detached` means no
frontend holds it.

`list` (`ls`) shows shell details, local mounts and forwards, and read-only
incoming resource leases. The group header identifies the managing host.
Connection endpoints and forwarding directions appear in `DETAIL`. Both views include
remote shell observations and open no SSH connections. The tree shows each
shell UID once and favors a live registration over observations of that UID.
`PARENT` shows the parent's short ID. For remote observations the list displays
the last recorded parent and includes the observation time in `DETAIL`. Its
resource status probes run under a separate time budget. `list --json`
also includes the tree. In it, `parent_row` is a one-based row number, and
roots omit it.

Invalid remote participation history makes the inventory incomplete. Parent edges
for registered tasks come directly from the daemon; no ancestry files are loaded.
Diagnostics identify the unavailable source, and invalid records report the
file, entry and rejected field. Resource leases require a task UID and direction.
A shell connection refreshes the known shells against the host’s live list.
The CLI, the root prompt and the attached terminal resolve names and ID prefixes
through the [task registry selector](task_selection.go). An explicit UID stays
bound across renames. The selector limits attach choices to idle roots and the
caller's detached direct children. For `attach HOST:`, the host supplies eligible
roots during SSH setup and the client ranks them by detach history.
A client can use a remote ID without running `list` first. See
[inventory and tree rendering](inventory.go) and [daemon metadata](daemon_metadata.go).

## Build and validate

Use Go and Git from the shared PATH and check the Go version against
[go.mod](go.mod). From the repository root, run
`go run ./scripts go build -o dist/sshore .` on Linux or target
`dist/sshore.exe` on Windows. The script applies the [SFTP](scripts/patches/pkg-sftp-v2.patch)
and [WinFsp](scripts/patches/go-winfsp.patch) patches to temporary copies of
the pinned modules and builds in a temporary Go workspace. Use the same entry point for tests and vet.

The permanent unit suite covers terminal byte handling, SSH option parsing
and filtering of terminal output. Run it with `go run ./scripts go test ./...`.
Go tests that drive a built product or installer live under `tests/integration`
and use the `integration` build tag. Installer checks on Windows use the
[PowerShell harness](tests/install_windows_test.ps1).

Use an ad hoc check for ordinary feature work. Keep scripts, temporary
regressions and fixture files outside the checkout and discard them after
validation. Keep permanent integration scenarios when they protect a shared
lifecycle or trust boundary such as shell ownership, release matching,
file integrity or explicit approval. Exercise those contracts through the
CLI or installer in isolation. OS-specific validation uses the affected OS
and its real terminal or mount driver. Keep real account shells out of tests
that simulate upgrade confirmation or reset.

- The Windows Server 2025 CI job runs the native ConPTY detach and reattach
  scenario against the built Windows executable. A disposable Ubuntu WSL 2
  distribution runs the Linux installer and Linux client integration suite
  against real OpenSSH, including the FUSE mount test. Separate Linux accounts
  isolate daemon ownership. The Windows client uses the same OpenSSH server to
  verify port forwarding in both directions. With WinFsp 2.2, it reads a
  WSL-created file on a Windows drive letter, verifies a Windows replacement
  from WSL, and checks unmount. The official WinFsp suite checks cached reads
  and writes, append and directory enumeration on that drive. Run the fixture
  only on a disposable Windows runner with a dedicated WSL distribution.

The [Tests workflow](.github/workflows/tests.yml) runs on demand and when a
push to `main` changes the integration tests or this workflow.

## Release

1. Update migration consequences in the [publisher](scripts/publish.go) when needed.
2. Run the relevant checks and push to `main`. [CI](.github/workflows/ci.yml)
   builds Linux and Windows amd64 archives and checksums as short-lived artifacts.
3. At the intended commit, run `git tag vX.Y.Z` and
   `git push origin vX.Y.Z`. [Publish](.github/workflows/publish.yml)
   builds and publishes both archives and their `SHA256SUMS` in one command,
   using the pushed tag as the binary version.
4. Verify the published commit/tag, both archives and their checksums before
   pruning older releases in the same major/minor series. Retain the highest
   published patch and all Git tags. Exclude drafts, prereleases and noncanonical
   tags from pruning. Published tags and assets are immutable.

Run `go run ./scripts` to build development packages using the nearest
full release tag for managed connections. The single-binary build above
reports `dev`.

## Planned work

Directory synchronization (`pull` / `push`) remains a proposal. Revisit deletion
opt-in, conflict handling and optional watching before implementing it.
