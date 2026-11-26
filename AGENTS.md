# Contributor constraints

- Keep implementations and workflows simple. Avoid speculative fallbacks and redundant checks.
  The publisher handles release failures.
- Treat Linux and Windows 11 / Server 2025 or newer as core amd64 client and host platforms.
  Record deferred platform gaps in the architecture TODOs. Split panes and tiling are out of scope.
  See [platform scope](docs/ARCHITECTURE.md#terminal-and-mux-design-scope).
- Manage only the authenticated account daemon and its registered tasks. Verify process identity
  before signaling. Never kill by process name or adopt unregistered processes. Require exact full
  release versions for normal connections. Follow the [update, reset, and approval safeguards](docs/SECURITY.md).
- Keep published Git tags immutable. Verify published assets before pruning older releases.
  Follow the [release workflow](docs/RELEASE.md).
- Make repository changes and create commits in the local checkout, then push those same
  commits using Git and the configured remote. Use Git to fetch and synchronize branches.
  Never use a connector or remote API to create commits, edit tracked files, or advance branches.
- For other GitHub operations, try the GitHub connector first. If it cannot complete the
  operation, try Git over SSH where applicable and report any remaining blocker.
- Use the shared toolchain from PATH and check versions against repository requirements. Get
  approval before substituting private runtimes, lowering requirements, or installing tools.
  For browser checks, use global Playwright and system Chrome. Do not download browsers.
- Treat integration tests as executable examples of the design. Name scenarios for the behavior
  they promise and show the setup, action, and observable outcome. Cover normal workflows and
  meaningful boundaries such as ownership, consent, failure, and cleanup. Explain a non-obvious
  invariant next to the step that exercises it.
- When changing a lasting contract, add or update its integration example alongside the change.
  Start from the example when it helps clarify the design. Reserve unit tests for core,
  side-effect-free code. Use temporary checks for ordinary changes and discard them afterward.
  Avoid duplicate cases and assertions tied to incidental wording. See [build and validation](docs/BUILD.md).
- Define CLI grammar once. Parsing and help must leave services and state untouched. Make
  destructive consequences and confirmations explicit.

## Documentation

- Keep only `README.md` and `AGENTS.md` at the root among documentation files.
  Keep the license at `LICENSE.txt` and organize other documentation under [docs/](docs/).
- Cap each project document in `docs/` at 200 lines. Only `SECURITY.md` and `GLOSSARY.md` may
  exceed that cap. Aim for 100 columns per line, except in `SECURITY.md`. Keep third-party
  license notices verbatim.
- Cut redundant prose and link to code and tests for implementation details. Use CLI help
  for command syntax and defaults. Update the relevant docs and diagrams when behavior changes.
- Show the user the exact proposed `README.md` edits for review and wait for explicit approval.
  Apply only the approved edits.
