# Project constraints

## Supported platforms

Support Linux and Windows on amd64 (x86-64) only. Do not build, package, or automatically install ARM targets. Keep published Git tags immutable.

Windows mux is local-only; do not implement Windows remote shell hosting.
Mux sessions have one terminal each; split panes and tiling are out of scope.
See [terminal and mux design scope](SSHORE.md#terminal-and-mux-design-scope).

## Version and setup policy

- Require exact full release versions, including PATCH. Do not introduce a separate protocol compatibility counter or silently accept a different release.
- The managed scope is the current account's unique daemon and its registered tasks. Query its authenticated loopback endpoint; do not scan for daemons or adopt unregistered processes. Preparation helpers and transient transports are not persistent shells.
- A newer installed program or daemon rejects an older client. Never automatically downgrade. Unknown/unordered versions allow manual recovery only.
- Missing software may be installed automatically. Existing older installations/shells require the user to type `update` after reviewing the affected shell list. Prepare and verify the replacement before ending any tasks; recheck the daemon task snapshot under the account setup lock. Request shutdown through the authenticated endpoint and wait for release; refusal, unavailability or timeout stops the update without forced fallback.
- Manual recovery preserves the authenticated SSH connection and provides a transient PTY shell. No persistence, approval execution, file transfer, dynamic forwarding, or local mux shortcuts (Ctrl+B) detach. Retain help/version/diagnostics; restore normal functionality only after reconnecting.
- New binaries honor SSHORE_DISABLED in recovery mode; remove inherited helper socket environment. This is a product-mode restriction, not a sandbox: arbitrary legacy binaries and deliberately escaped/reparented tasks cannot be globally controlled.
- Signal only confirmed, same-user shell processes and their identified tasks, using process identity validation. Never use process-name-wide kill commands. Failure to prepare a replacement must not terminate tasks.
- Published tags are immutable. After a successful new publication and complete replacement assets, retain only the highest published PATCH in each MAJOR.MINOR series. Delete older release objects/assets, never their Git tags. Skip drafts, prereleases and noncanonical tags; failed or incomplete publication must not prune releases.

Use GLOSSARY.md terminology for code organization. Name business packages and
file groups after public commands (`cp`, `mount`, `forward`, `review`); `cp`
and `mount` share the flat `internal/files` package. Keep
shared infrastructure named for its responsibility. `internal/tmux` owns terminal
and shell mechanisms shared by launch, spawn, attach and detach. Keep command
policy, review records, forward lifecycle, private application state and release
version policy in the main program; do not split out packages solely for small helpers.

## File cohesion

- Keep modules highly cohesive and loosely coupled: one clear responsibility,
  explicit ownership and small interfaces. Extend existing modules before adding abstractions.
- Use `tests/` for product-level black-box tests, grouped by user workflow. Exercise the CLI, protocols or public APIs to cover cross-platform behavior, compatibility, integration and user-visible regressions. Keep module-level tests and regressions beside their owning packages; do not expose production internals or use overlays to relocate them.
- Merge small, simple helpers into their concept's module; retain separate files when platform or build constraints require them.
- Consolidate small native adapters by module and supported platform (`*_linux.go` / `*_windows.go`); keep test-only build constraints separate.
- Group tests by module behavior and functional boundaries, not by source file,
  helper or individual regression. Preserve platform and build-tag isolation.
- Use 200–500 lines per file as a guide, not a quota; cohesion takes priority.
  Merge related fragments and split only for independent responsibilities or complexity.
- Keep the module ownership map in SSHORE.md current.

## CLI ownership

- Define command options, aliases, value types, arity and shared conflicts once;
  parsing must consume those definitions. Keep command-specific
  semantics in their command adapters, and keep SSH suffix policy separate.
- Parsing and help/example output have no execution side effects: no SSH,
  authentication, daemon probes/startup, state mutation or ID allocation.

## User interaction

Keep user-facing prompts concise, effective and elegant. Lead with the result or
required action; show only information needed to understand the current state or
decide what to do next. Prefer compact, aligned tables for lists. Avoid redundant
status logs, repeated instructions and explanatory boilerplate; put diagnostics
in explicit detail views and command reference material in CLI help. Preserve
necessary warnings, destructive consequences, confirmation requirements and
recovery instructions. Distinguish cached or uncertain state from live results.
Apply this style consistently across commands.

## Documentation ownership

The project's documentation consists **only** of these five files at the
repository root. Do not create additional project documents, nested `docs/`
trees, component READMEs, design notes or work logs elsewhere.

| File | Sole responsibility |
| --- | --- |
| `README.md` | Project introduction and short user manual: installation, common workflows, essential consequences and links to command help. |
| `SSHORE.md` | Development guide: architecture and ownership summaries, design constraints, build, validation and release workflow. |
| `SECURITY.md` | Security model: trust boundaries, threats, approval/update safeguards, recovery restrictions, guarantees and remaining risks. |
| `GLOSSARY.md` | Definitions of project terms, identities and their relationships; use these terms consistently. |
| `AGENTS.md` | Contributor/agent rules, project constraints and documentation ownership. |

Keep documentation in these five files; do not add component READMEs, design
notes, work logs or changelogs. Release notes are generated by `scripts/release.sh`.
CLI help owns syntax, options and defaults. Code and tests own implementation
details; comments explain only local intent or invariants.

Maintain one authoritative source per fact and link instead of duplicating it.
Update the owning document when behavior changes. Keep README short while
preserving essential consequences, including updates ending shells.

Keep the Mermaid diagrams in README and SECURITY current. README explains the
user's connection/shell/approval flow; SECURITY explains trust boundaries and
security-relevant gates. Update affected diagrams with lifecycle changes and
validate their syntax. After documentation edits, check relative links and code
fences, including links in packaged documents. Do not alter published release
assets to update their documentation.

## Validation

Keep new tests small and focused; do not add heavyweight end-to-end suites or extra versioned builds. Run the existing checks appropriate to the change. Lifecycle integration test executables must use `-tags=sshoretest`, which compiles in runtime-directory isolation; never run simulated upgrade confirmation against the developer's real account shells. Release binaries MUST NOT use that tag.

For routine changes, run only the relevant existing tests. Use the full core Go
suite for cross-component changes; reserve race, real SSH, native mount and
installer checks for changes affecting those areas. Do not repeat successful
checks or add a validation layer solely for process completeness.
