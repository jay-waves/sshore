# sshore development

## Architecture and code map

sshore resolves client configuration and establishes SSH connections in Go.
`internal/sshconfig` evaluates file rules without opening connections.
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
| Where is behavior tested? | Module and CLI tests beside the implementation, [the OpenSSH integration test](tests/integration/) and the [loopback SSH fixture](tests/testutil/sshtest/) |

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

A root shell follows the authenticated frontend lease. A same-host child
follows its registered parent task. For a child on another host, the initiating
daemon owns an SSH lease scoped to the parent. Explicit detach sets a pin on the
same task before releasing input. Internal switches and approval handoffs release
input under the original scope. The daemon serializes pin expiry, scope loss and
reattachment under the task lock. When the scope ends, an effective pin keeps the
shell and its dependent descendants running. See [retention](shell_retention.go),
[transport](shell_transport.go) and [attachments](shell_attachment.go).

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

Distinguish live observations, cached records and unknown state. Saved ownership
and attachment records provide display context. Only the account daemon's
registry establishes management authority. The live inventory takes one snapshot
of registered shells and resources. Resource status probes run
after the snapshot within their own time budget. For `pstree`, the daemon reads
registered tasks, saved tree metadata and incoming transport leases independently
and returns a normalized, sorted tree.
Branches show live parent shells. Each shell row shows its own current
attachment state and `*` marks the shell using the current terminal. Siblings
are sorted by task ID, so detach and reattach do not move them in the tree.
Shell and resource arrows appear on their task lines. The daemon appends remote
shells last observed detached as roots, most recent first.
For `list`, the daemon combines registered tasks, active incoming resources and
its records of remote shells used from this account. The list is available offline.
Plain and JSON output use the same task data. The table shows when remote
shells were seen and marks read-only access in DETAIL. The daemon uses `active`
for attached shells, running workers, and live incoming leases. It uses
`unknown` when a state cannot be confirmed. JSON uses the same `state` field for shells, resources and peers and
includes `observed: true` and `seen_at` on recorded remote shells.
Invalid shell-tree or participation history makes the inventory incomplete.
Diagnostics identify the unavailable source, and invalid records report the
file, entry and rejected field. Resource leases require a task UID and direction.
A shell connection refreshes the known shells against the host’s live list.
The local daemon resolves implicit attach targets from detach history and live
local registrations. For `HOST:ID`, the named host resolves the selector against
its current registrations.
A client can use a remote ID without running `list` first. See
[inventory and tree rendering](inventory.go) and [daemon metadata](daemon_metadata.go).

## Build and validate

Use Go and Git from the shared PATH and check the Go version against
[go.mod](go.mod). From the repository root, run
`go run ./scripts go build -o dist/sshore .` on Linux or target
`dist/sshore.exe` on Windows. The script applies the [SFTP patch](scripts/patches/pkg-sftp-v2.patch)
to a temporary copy of the pinned module and runs the build in a temporary
Go workspace. Use the same entry point for tests and vet.

| Change | Validation |
| --- | --- |
| Command or module | Run the owning package's tests with `-run`. For broad changes use `go run ./scripts go test -count=1 -timeout=5m ./...`. For race checks that include subprocess builds, set `GOFLAGS=-race`. |
| SSH or remote setup | Use the [loopback Go fixture](tests/testutil/sshtest/) for protocol tests and `tests/integration/ssh_docker.sh` for a real OpenSSH check in a disposable Linux amd64 container. |
| Terminal or native mount | Exercise the affected OS. For terminal handoff check pending input, redraw, interrupts and cleanup across detach/reattach. On a FUSE/macFUSE/WinFsp client, set `SSHORE_TEST_FTP_MOUNT=1` and run `go run ./scripts go test -count=1 -run '^TestFTPNativeMount$' ./internal/files`. |
| Installer or release | On Linux, run `go run ./scripts go test -run TestScripts .`. Cases and fixtures live in [testdata/cli](testdata/cli/). |
| Documentation | Check relative links, anchors, fences and affected diagrams in the root documents. |

The configuration resolver applies command-line options before user and system
files, preserves earlier values on a final pass, and keeps file lists as
separate entries. Its supported grammar can be extended independently of the
transport. The Go tests check behavior recorded against OpenSSH, and the Docker
fixture verifies connections against a real `sshd`.

Go lifecycle fixtures use temporary state directories and re-execute the test
binary. Use a normal build when separate OS accounts isolate client and server
daemon ownership. Never simulate upgrade confirmation against real account shells.
The manual [Tests workflow](.github/workflows/tests.yml) runs Linux and Windows
validation independently of builds and publication.

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
