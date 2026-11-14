# Project constraints

## Supported platforms

Support Linux and Windows on amd64 (x86-64) only. Do not build, package, or automatically install ARM targets. Keep existing published releases immutable.

## Version and setup policy

- Require exact full release versions, including PATCH. Do not introduce a separate protocol compatibility counter or silently accept a different release.
- The managed scope is the current remote Unix account and its visible sshore session daemons, across runtime directories. Other users are excluded. Preparation helpers and transient transport processes are not persistent sessions.
- A newer installed program or daemon rejects an older client. Never automatically downgrade. Unknown/unordered versions allow manual recovery only.
- Missing software may be installed automatically. Existing older installations/sessions require the user to type `update` after reviewing the affected session list. Prepare and verify the replacement before ending any tasks; recheck the process snapshot under the account setup lock.
- Manual recovery preserves the authenticated SSH connection and provides a transient PTY shell. No persistence, approval execution, file transfer, dynamic forwarding, or Ctrl-] detach. Retain help/version/diagnostics; restore normal functionality only after reconnecting.
- New binaries honor SSHORE_DISABLED in recovery mode; remove inherited helper socket environment. This is a product-mode restriction, not a sandbox: arbitrary legacy binaries and deliberately escaped/reparented tasks cannot be globally controlled.
- Signal only confirmed, same-user session processes and their identified tasks, using process identity validation. Never use process-name-wide kill commands. Failure to prepare a replacement must not terminate tasks.
- Published tags are immutable. Publishing a release MUST preserve older releases and assets; no automatic release deletion.

## Documentation ownership

The project's documentation consists **only** of these five files at the
repository root. Do not create additional project documents, nested `docs/`
trees, component READMEs, design notes or work logs elsewhere.

| File | Sole responsibility |
| --- | --- |
| `README.md` | Project introduction and short user manual: installation, common workflows, essential consequences and links to command help. |
| `SSHORE.md` | Development document: architecture, component relationships, protocol/lifecycle design, implementation tradeoffs, build, validation and release workflow. |
| `SECURITY.md` | Security model: trust boundaries, threats, approval/update safeguards, recovery restrictions, guarantees and remaining risks. |
| `GLOSSARY.md` | Definitions of project terms, identities and their relationships; use these terms consistently. |
| `AGENTS.md` | Contributor/agent rules, project constraints and documentation ownership. |

Everything else is code, configuration, tests, tooling, generated output or legal
material, **not a project documentation source**. Source/script/workflow comments
may explain a local invariant, algorithm or execution requirement, but must not
serve as standalone architecture or development documentation. Move such design
explanations to `SSHORE.md`; keep only useful local comments near the code.
CLI help remains the executable command reference for syntax, options and defaults;
README should not duplicate it. License notices remain legal material. Generated
release notes belong to releases, with their generation in `scripts/release.sh`;
do not add a separate changelog or release-specific work log.

Keep each explanation in its owning document and link to it instead of copying
it. Update the relevant document when behavior changes. README must stay short;
retain consequences users need to decide, such as updates ending sessions, while
moving mechanisms to SSHORE and security rationale to SECURITY. New terminology
belongs in GLOSSARY, not a competing definition in another file.

Keep the Mermaid diagrams in README and SECURITY current. README explains the
user's connection/session/approval flow; SECURITY explains trust boundaries and
security-relevant gates. Update affected diagrams with lifecycle changes and
validate their syntax. After documentation edits, check relative links and code
fences, including links in packaged documents. Do not alter published release
assets to update their documentation.

## Validation

Keep new tests small and focused; do not add heavyweight end-to-end suites or extra versioned builds. Run the existing checks appropriate to the change. Lifecycle integration test executables must use `-tags=sshoretest`, which compiles in runtime-directory isolation; never run simulated upgrade confirmation against the developer's real account sessions. Release binaries MUST NOT use that tag.
