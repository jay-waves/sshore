# Contributor constraints

- Keep implementations and workflows simple. Avoid speculative fallbacks and redundant checks.
  The publisher handles release failures.
- Support Linux and Windows amd64 clients. Support remote shells and services on Linux amd64,
  and remote SFTP mounts on NTFS and ReFS on Windows amd64. Windows remote shell hosting is
  experimental. Split panes and tiling are out of scope.
  See [platform scope](docs/ARCHITECTURE.md#terminal-and-mux-design-scope).
- Manage only the authenticated account daemon and its registered tasks. Verify process identity
  before signaling. Never kill by process name or adopt unregistered processes. Require exact full
  release versions for normal connections. Follow the [update, reset, and approval safeguards](docs/SECURITY.md).
- Keep published Git tags immutable. Verify published assets before pruning older releases.
  Follow the [release workflow](docs/RELEASE.md).
- For GitHub operations, try the GitHub connector first. If it cannot complete the operation,
  try Git over SSH. If SSH also fails or cannot perform the operation, stop and report the
  operation and the specific failure from each attempt.
- Use the shared toolchain from PATH and check versions against repository requirements. Get
  approval before substituting private runtimes, lowering requirements, or installing tools.
  For browser checks, use global Playwright and system Chrome. Do not download browsers.
- Reserve unit tests for core, side-effect-free code. Verify ordinary changes with temporary
  checks and discard them afterward. Cover lasting system contracts with external integration
  tests. See [build and validation](docs/BUILD.md).
- Define CLI grammar once. Parsing and help must leave services and state untouched. Make
  destructive consequences and confirmations explicit.

## Documentation

- Keep only `README.md`, `LICENSE`, and `AGENTS.md` at the root among documentation files.
  Organize the remaining documentation by topic under [docs/](docs/).
- Cap each project document in `docs/` at 200 lines. Only `SECURITY.md` and `GLOSSARY.md` may
  exceed that cap. Aim for 100 columns per line, except in `SECURITY.md`. Keep third-party
  license notices verbatim.
- Cut redundant prose and link to code and tests for implementation details. Use CLI help
  for command syntax and defaults. Update the relevant docs and diagrams when behavior changes.
- Show the user the exact proposed `README.md` edits for review and wait for explicit approval.
  Apply only the approved edits.
