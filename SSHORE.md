# sshore development

## Architecture and code map

sshore uses system OpenSSH to evaluate client configuration (`ssh -G`) and Go to
establish connections. The root package owns command policy, authorization and
task lifecycles. Internal packages implement transport, terminal and filesystem
operations. For exact behavior, start at the command entry point and follow it
to the platform implementation and its tests.

| Question | Start here |
| --- | --- |
| How is a command parsed and dispatched? | [Command definitions and help](cli_help.go), [parser](cli_schema.go), [dispatch](cli.go) |
| How does a client connect and authenticate? | [Client connections](client_connection.go), [SSH transport and host verification](internal/ssh/) |
| How are installation and approval enforced? | [Setup](setup.go), [approval handling](review.go), [request validation](review_store.go) |
| How do copy, mount and forwarding reach their target? | [Resource commands](cli_resources.go), [shell routes](resource_route.go), [filesystem operations](internal/files/), [forwarding](forward.go) |
| Where is behavior tested? | Module tests beside the implementation, [public CLI workflows](tests/) and [real-SSH scenarios](tests/integration/) |

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
Their entry points supply owner supervision and the commands specific to
local handoff or hosted resources.

An experimental Apple Silicon client can be built from source with
`GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o sshore .`. It targets
SSH connections to Linux hosts. Local shell hosting, local mounts and the
automatic local installer are unavailable. Cross-compilation alone does not
validate macOS terminal input, SSH authentication or daemon lifetime.

### Shell ownership and attachment

A spawned shell depends on its original parent and, for a remote spawn, its
lifetime SSH connection. A launched shell survives its launching client.
Detaching or switching changes who interacts with the shell. Its original
lifetime rules still apply. During handoff, release the old input reader before
giving its successor the terminal. See [spawn transport](spawn_transport.go),
[Linux launch](launch_daemon_linux.go) and [attachment handling](shell_attachment.go).

### Account daemon and mounts

One authenticated daemon per OS account owns registered shells and resources.
Normal connections require the exact full release version. Validate process
identity before signaling and preserve approval checks for shell-routed resources.
Mount and forward configuration travels in the authenticated launch request.
The upgraded stream carries readiness and the caller's commit. The mount worker
uses a separate pipe exchange because it can outlive the launch connection.
Trace changes through the [task registry](daemon_tasks.go), [mount coordinator](mount.go)
and [route authorization](resource_route_linux.go). Consult [SECURITY.md](SECURITY.md)
before changing update, reset or approval behavior.

### Inventory and diagnostics

Distinguish live observations, cached records and unknown state. Saved ownership
and attachment records provide display context. Only the account daemon's
registry establishes management authority. The live inventory takes one snapshot
of registered shells and resources. Resource status probes run
after the snapshot within their own time budget. Clients add history and peer
observations separately. Linux hosted-shell exit receipts use
the private account-daemon directory. Older runtime-directory receipts remain
visible until they expire. See [inventory](inventory.go),
[tree rendering](pstree.go) and [daemon metadata](daemon_metadata.go).

## Build and validate

Use Go from the shared PATH and check its version against [go.mod](go.mod).
Build with `go build -o dist/sshore .` on Linux or use `dist/sshore.exe` on
Windows. Choose the smallest check that exercises the changed behavior.

| Change | Validation |
| --- | --- |
| Command or module | Run the owning package's tests with `-run`. For broad changes use `go test -count=1 -timeout=5m ./...`. For race checks that include subprocess builds, set `GOFLAGS=-race`. |
| SSH or remote setup | Use the [loopback Go fixture](tests/testutil/sshtest/) for protocol tests. Run local real-SSH checks in the disposable Linux amd64 Docker fixture with `tests/integration/ssh_docker.sh`. Use `--setup-only` for installation changes. |
| Terminal or native mount | Exercise the affected OS. For terminal handoff check pending input, redraw, interrupts and cleanup across detach/reattach. On a FUSE/WinFsp host, set `SSHORE_TEST_FTP_MOUNT=1` and run `go test -count=1 -run '^TestFTPNativeMount$' ./internal/files`. |
| Installer or release | Run the corresponding tests under [tests/](tests/). |
| Documentation | Check relative links, anchors, fences and affected diagrams in the packaged documents. |

Go lifecycle fixtures use temporary state directories and re-execute the test
binary. Use a normal build when separate OS accounts isolate client and server
daemon ownership. Never simulate upgrade confirmation against real account shells.
The manual [Tests workflow](.github/workflows/tests.yml) runs Linux and Windows
validation independently of builds and publication.

## Release

1. Set [VERSION](VERSION) and the default in [main.go](main.go) to the same full
   version. Update the release notes and migration consequences in the
   [publisher](scripts/release.sh).
2. Run the relevant checks and push to `main`. [CI](.github/workflows/ci.yml)
   builds Linux and Windows amd64 archives and checksums as short-lived artifacts.
3. At the intended commit, run `git tag "v$(cat VERSION)"` and
   `git push origin "v$(cat VERSION)"`. [Publish](.github/workflows/publish.yml)
   rebuilds the tagged source and checks the tag against its version.
4. Verify the published commit/tag, both archives and their checksums before
   pruning older releases in the same major/minor series. Retain the highest
   published patch and all Git tags. Exclude drafts, prereleases and noncanonical
   tags from pruning. Published tags and assets are immutable.

## Planned work

Directory synchronization (`pull` / `push`) remains a proposal. Revisit deletion
opt-in, conflict handling and optional watching before implementing it.
