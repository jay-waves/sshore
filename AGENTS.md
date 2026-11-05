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

## Validation

Keep new tests small and focused; do not add heavyweight end-to-end suites or extra versioned builds. Run the existing checks appropriate to the change. Lifecycle integration test executables must use `-tags=sshoretest`, which compiles in runtime-directory isolation; never run simulated upgrade confirmation against the developer's real account sessions. Release binaries MUST NOT use that tag.
