#!/usr/bin/env bash
# Publish a full semantic version after both main-branch release builds pass.
# CI builds only Linux/Windows amd64 without sshoretest. Full validation lives in
# the separate workflow_dispatch Tests workflow and never gates release builds.
# Published tags never change. Existing releases are never overwritten.
# After successful publication, retain only the latest complete patch per x.y.
set -euo pipefail

version=$(cat VERSION)
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
tag="v$version"

head=$(gh api "repos/$GH_REPO/git/ref/heads/main" --jq '.object.sha')
if [[ "$head" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: this commit is no longer the main branch head."
  exit 0
fi

existing_ref=$(gh api "repos/$GH_REPO/git/matching-refs/tags/$tag" \
  --jq ".[] | select(.ref == \"refs/tags/$tag\") | .object.sha")
if [[ -n "$existing_ref" ]]; then
  # The ref SHA is a tag-object SHA for annotated tags. The commits endpoint
  # dereferences both annotated and lightweight tags to their commit.
  existing_commit=$(gh api "repos/$GH_REPO/commits/$tag" --jq '.sha')
  if [[ "$existing_commit" != "$GITHUB_SHA" ]]; then
    echo "Skipping release: $tag already belongs to another commit; bump VERSION to publish again."
    exit 0
  fi
else
  gh api --method POST "repos/$GH_REPO/git/refs" \
    -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" > /dev/null
fi

cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<NOTES
sshore $version

Includes Linux and Windows binaries for amd64 (x86-64) only.
Verify downloaded archives against SHA256SUMS.
Commit: $GITHUB_SHA
NOTES
if [[ "$version" == "0.3.0" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Dynamic same-port loopback forwarding: `sshore forward PORT`, `--list`, and `--stop PORT`.
- Forwarding belongs to the current connection; detach/disconnection closes listeners and connections.
- Native SSH forwarding and diagnostics: `-L`, `-R`, `-D`, `-C`, `-E`, and `-vv`/`-vvv`.
- Attached short-option values such as `-p2222` and `-R9000:localhost:3000`.

Upgrade: protocol 3 requires matching client and server binaries. Exit old
persistent sessions with the old client before upgrading; old daemons cannot
upgrade in place. Linux servers; Linux and Windows clients.
NOTES
fi


if [[ "$version" == "0.3.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Remote file and port requests now queue a local approval and detach, preserving the remote shell.
- Run `sshore approve` locally to review and execute once; SSH authentication remains with system OpenSSH.
- `--attach` (default) resumes the remote shell; `--stay` stays local, keeping port tunnels in the foreground until Ctrl-C.
- List or reject pending requests with `approve --list` and `approve ID --reject`.
- Requests expire after 10 minutes. Unsolicited file/port operations cannot bypass approval; `--download-dir` only sets defaults.
- Added SECURITY.md with the trust model, attacker assumptions, protections, and remaining limits; included in release archives.

Upgrade: despite the patch version, this release changes the wire protocol to 4.
Update both client and server. Exit existing sessions with the old client before
upgrading; running protocol-3 daemons cannot upgrade in place. Batch downloads
now require one approval and execute sequentially, stopping on the first failure.
NOTES
fi


if [[ "$version" == "0.3.2" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Added a user-run Linux amd64/arm64 installer: architecture detection, pinned or latest releases, HTTPS downloads, SHA256 verification, atomic installation, and old-version backup.
- Missing remote executables now print an installation command matching the local client. No automatic installation is performed.
- Default remote lookup falls back to ~/.local/bin/sshore when it is absent from PATH; explicit --server paths remain authoritative.
- Added installer, remote-probe, and real-SSH regression coverage, plus installation trust-boundary documentation.
- Fixed Ctrl-C in foreground `approve --stay` tunnels so normal cancellation does not race with SSH process termination.

Install on Linux:
```sh
curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/v0.3.2/install.sh | sh -s -- --version 0.3.2
```

Protocol remains 4, compatible with 0.3.1 session daemons. When upgrading from
0.3.0 or earlier, exit old sessions with the old client and update both ends.
NOTES
fi

if [[ "$version" == "0.3.3" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Clear mouse/focus reporting and bracketed paste when returning to the local terminal, including approval boundaries; preserve Unix TTY and Windows Console settings.
- Track standard mouse modes while detached or approval output is muted, restoring them when terminal interaction resumes.
- Request a foreground-job redraw on reattachment even at unchanged dimensions; approved operations wait until output is enabled, and --stay remains local.
- Keep remote output transparent, with bounded mouse tracking rather than full terminal emulation.
- Add terminal cleanup, split-sequence parsing, foreground redraw, and multiplexer reconnection regression tests.

Protocol remains 4. Update client and server and create a new remote session
for all fixes: already-running daemons retain their original code. Redraw is
best effort; terminal-specific passthrough and full screen replay are not emulated.
NOTES
fi

if [[ "$version" == "0.3.4" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add a Windows PowerShell installer for amd64/arm64, with optional INSTALL_DIR and pinned or latest release selection.
- Verify SHA256SUMS and the executable version before replacement. PATH, execution policy and PowerShell profiles remain unchanged; setup instructions are printed only.
- Document user responsibility for installation permissions, closing running executables, backups, PATH configuration and compatible client/server versions.
- Consolidate terminal recovery tests and remove emulator-specific CI dependencies, retaining protocol, approval, transfer integrity and terminal recovery coverage.
- Add offline installer checks to Windows PowerShell 5.1 and PowerShell 7 CI.

Install the latest Windows client:
```powershell
irm https://raw.githubusercontent.com/jay-waves/sshore/v0.3.4/install.ps1 | iex
```

Default directory: %LOCALAPPDATA%\sshore\bin. Set $env:INSTALL_DIR to override it.
Protocol remains 4. Running remote session daemons are not upgraded or stopped.
NOTES
fi


if [[ "$version" == "0.3.5" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add `sshore connect HOST` to connect unambiguously to SSH aliases that match subcommands.
- Fix inherited Windows mouse capture in remote shells while preserving mouse input for remote TUIs.

Protocol remains 4. Update the Windows client to receive the mouse fix.
NOTES
fi

if [[ "$version" == "0.3.6" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add local Tab path completion to upload and download approval prompts.
- Start single-file uploads when the local source path is submitted, without a separate yes prompt; Ctrl-C cancels.
- Preserve final confirmation for downloads, forwarding and batch requests, and release the line editor before reconnecting.

Protocol remains 4. Update the local client to use the new prompts.
NOTES
fi

if [[ "$version" == "0.3.7" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Connections without -s create or resume the default main session.
- Explicit -s NAME continues to create or resume a separate named session.

Protocol remains 4. Update the client for default-session reuse.
NOTES
fi

if [[ "$version" == "0.4.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Require one exact active version per remote account; reject newer remote versions without downgrading.
- Automatically prepare a verified remote executable when missing or older. Updating an existing installation requires explicit confirmation before ending listed old sessions and tasks.
- Offer a nonpersistent recovery shell on the same SSH connection for manual maintenance, with sshore features disabled.
- Check running daemon versions, report background sessions, and serialize managed setup operations.
- Unify status colors and UTF-8 path handling; improve default session reuse and local path input.
- Preserve previous GitHub releases and assets when publishing new versions.

Updating old sessions terminates their tasks. Press Enter at setup to keep them
and use manual recovery instead. A matching published binary must be available
for automatic preparation; source/custom builds may need manual installation.
Checksums verify downloaded bytes, not a compromised release publisher.
NOTES
fi

if [[ "$version" == "0.4.2" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Fix a Windows panic when displaying setup confirmation and local approval prompts by keeping readline prompts uncolored.
- Preserve explicit update confirmation and exact client/server version matching.
NOTES
fi

if [[ "$version" == "0.4.3" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Fix Windows setup and approval confirmation exiting with "The operation completed successfully" instead of continuing.
- Simplify setup prompts, hide temporary installer output on success, and clearly report affected sessions and completed updates.
- Explain when remote state changes require fresh confirmation; retain explicit update approval and exact version matching.
- Move full validation to the manually triggered Tests workflow so release builds no longer wait for tests.
NOTES
fi

if [[ "$version" == "0.4.4" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Bound local path completion lists to the terminal size; large lists ask for more input instead of flooding the screen.
- Cycle small lists with Tab and accept selections with Enter before submitting; color directories using the terminal palette.
- Use native Windows VT output for colored prompts and completion, with uncolored fallback and console-mode restoration.
- Simplify the README, refresh architecture and security diagrams, move implementation notes into source headers, and document these conventions in AGENTS.md.
NOTES
fi

if [[ "$version" == "0.4.5" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Restore automatic Pokemon session names: each connection without -s creates a new session, allowing multiple independent connections to one host.
- Reserve names for attached and detached sessions; use -s NAME to resume, or choose a custom name when the 24 automatic names are occupied.
- Show bundled color sprites for 24 familiar Pokemon, with credits and license notices for pokemon-colorscripts included in release packages.
- Hide sprites with -q or NO_COLOR, or on non-color terminals; preserve recovery-shell restrictions and explicit session approval checks.

Reconnect to existing work with the session name printed on connection:
`sshore HOST -s NAME`. A bare `sshore HOST` now creates a new session.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update.
NOTES
fi

if [[ "$version" == "0.4.6" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Fix uploads hanging when a peer rejects a transfer without draining its stream.
- Send downloads from the same file handle used to verify approved metadata.
- Remove obsolete direct-transfer relays and workers; keep transfers on client-initiated approved streams.
- Simplify one-shot approval consumption and session cleanup, and move existing transfer tests to the current approval protocol.
- Route failed or unknown remote version probes through recovery setup instead of disconnecting or misclassifying versions.
- Preserve published releases and assets: publishing an existing version is now a no-op.
- Refresh automatic-session SSH tests and recovery documentation.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Recovery mode
preserves the SSH connection without executing pending approvals.
NOTES
fi

if [[ "$version" == "0.5.0" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Remove bundled Pokemon pixel artwork; retain automatic session names.
- Show session status and shell/task ancestry as an ASCII process tree, with PIDs and foreground markers.
- Only explicit detach preserves a remote session. Closing the local terminal or losing the connection ends the session when transport loss is detected, using normal SSH-style PTY hangup semantics.
- Keep explicit detach for approval requests and completed file operations with --stay; Ctrl-C in a --stay port tunnel now ends the remote session too.
- Drain cancelled transfer workers before daemon shutdown so incomplete uploads can be removed.
- Update connection failure messages, CLI help, lifecycle diagrams and regression coverage.

Breaking behavior change: unexpected disconnection no longer saves the session.
Use Ctrl-] or `sshore detach` before leaving work running. Existing transport
keepalives detect silent connection loss; there is no reconnect grace period.
Programs deliberately ignoring or escaping terminal hangup may survive, as with SSH.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.5.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Separate session commands: `sshore new HOST [NAME]`, `sshore list HOST`, and `sshore attach HOST [NAME]`.
- New only creates sessions and rejects occupied names; attach only resumes existing sessions and never takes over an attached client.
- Omit the attach name to resume the only detached session or choose from several on the same SSH connection. No detached sessions means an error, not a new shell.
- Add command-specific help, including `sshore new --help`, `sshore list --help`, `sshore attach --help`, and `sshore help COMMAND`.
- Print a copyable reconnect command after detach, preserving SSH connection options.
- Update documentation, lifecycle diagrams and targeted regression tests.

Breaking CLI change: `sshore HOST`, `sshore connect HOST`, `sshore HOST list`,
and `-s`/`--session` are no longer accepted. Use `new`, `list`, or `attach`.
Only explicit detach preserves remote work; exit or detected connection loss
ends the session, as in 0.5.0.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.5.2" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Dynamic forwards can target a different remote port or any host reachable from the SSH server, for example `sshore forward 7777:192.168.1.20:6666`.
- Keep local listeners on `127.0.0.1` and show the approved remote target in confirmation, status and list output.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.5.3" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Dynamic forwards can target a different remote port or any host reachable from the SSH server, for example `sshore forward 7777:192.168.1.20:6666`.
- Keep local listeners on `127.0.0.1` and show the approved remote target in confirmation, status and list output.
- Publish releases correctly when the version uses an annotated Git tag.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.5.4" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- End a named session and its identified tasks with `sshore kill HOST NAME`, using version and process identity checks.
- Rename sessions with `sshore rename HOST OLD NEW` while preserving the shell and inherited helper connections.
- Show foreground programs, shortened working directories, session ages and recent exit reasons.
- Resolve session names using private metadata and lifetime locks, without waiting on unrelated unresponsive daemons.
- Filter session choices with /text and select by #number or exact name; Enter cancels.
- Improve local approval path validation and prompts using the existing lightweight line editor, with no new runtime dependencies.
- Preserve terminal restoration errors and synchronize completion state during input handoff.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.6.0" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Consolidate commands into review, cancel, mount and list; mount startup identifies SFTP over SSH.
- Manage mounts and forwards as independent named background resources, with peer inventory and cancellation.
- Use explicit shell-relative from/to port forwarding; local forwards support SSH connection options and opt-in bounded retries.
- Add list --details/--json with bounded local status queries, transport state, recent failures and log paths. Target services are not probed.
- Add SFTP request/startup timeouts without replaying writes, and transfer speed, ETA and batch failure summaries.
- Share filtered request/session selection and simplify approval prompts.
- Require SSH options after -- across connection commands, mount and forward; generated reconnect commands use the same syntax.

Breaking CLI changes:
- approve becomes review; kill and stop become cancel; ftp becomes mount.
- Use list instead of per-command --list; use cancel approval/ID to reject requests.
- Place sshore options before -- and SSH options after it:
  `sshore forward work 8080 to 3000 --name web -- -J jump`.
- Native -L/-R/-D remain connection-level compatibility options after --,
  separate from independently managed forward resources.

Forward retries default to off and cannot restore existing TCP connections.
SFTP timeouts close the transport; remote writes may already have completed.
Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the list and type update. Existing releases
and assets remain unchanged.
NOTES
fi

if [[ "$version" == "0.6.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Unify file commands as `cp SOURCE... TARGET` and forwarding as `forward LISTENER SERVICE`.
- Use `origin:` (or `:`) for the attached client's endpoint; filled paths/ports remain editable suggestions in remote requests.
- Support direct copy pipelines: `sshore cp HOST:FILE -` writes stdout, and `sshore cp - HOST:FILE` reads stdin until EOF.
- Keep stdout byte-clean, stream without local spooling, and verify temporary uploads before no-overwrite publication. Stream copies accept exactly one remote file; stdout may contain partial data on failure.
- Start approved operations when all local paths or the local port are submitted, without another yes prompt.
- Share SFTP transfer code between direct copies and approved session streams; remove the custom file-transfer state machine.
- Simplify endpoint state, private metadata writes and SSH option handling. Keep core tests as the default and extended checks opt-in.

CLI migration: replace upload/download with cp, and from/to forwarding with
listener/service operands. See `sshore cp --help` and `sshore forward --help`.
Client and remote versions must match exactly. Request pending operations again
after upgrading. Updating older sessions ends their tasks only after you review
the affected session list and type update. Existing releases remain unchanged.
NOTES
fi

if [[ "$version" == "0.6.2" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Reduce repeated mount metadata and directory requests with bounded, short-lived caches and concurrent-request sharing.
- Add Linux fsnotify notifications over the mount's existing SSH connection; no second authentication or cgo is needed. Watch only accessed directories, with bounded subscriptions and TTL fallback.
- Avoid extra console windows for background Windows mount/forward SSH processes and allow concurrent WinFsp opens.
- Report remote filesystem capacity through statvfs and add opt-in mount request/cache/callback statistics with --stats.
- Preserve synchronous writes, cache invalidation on local changes, request deadlines and no automatic write replay.

Mount migration: default mounts now require the same full sshore release on the
Linux amd64 remote and an external OpenSSH sftp-server executable. Mount does
not install or update remote software. Use --server PATH for a custom executable,
or --no-watch for the previous plain SFTP subsystem mode. Notifications invalidate
the backend metadata cache; they do not implement Explorer's native auto-refresh
or guarantee freshness of already-open files and directory snapshots.

Client and remote versions must match exactly. Updating existing older sessions
ends their tasks only after you review the affected list and type update. Existing
releases and assets remain unchanged. Linux and Windows amd64 only.
NOTES
fi

if [[ "$version" == "0.6.3" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Run approved forwards with one SSH authentication, preserving source session version/name/PID and detached-state checks without reattaching the shell.
- Keep background Windows workers, SSH and askpass children in a hidden console.
- Show native SSH startup errors directly, including occupied or denied remote listening ports; close watcher stdin before forced shutdown.
- Restore Bash prompts on attach without injecting Enter or executing unfinished input.
- Prevent concurrent Windows reviews from consuming the same approval more than once.
- Restore recent-connection shortcuts: `sshore attach`, `sshore attach HOST`, and explicit `sshore attach HOST NAME`; remembered transport options never replay native forwarding rules.
- Include bounded mount metadata caches, same-connection fsnotify, remote capacity queries and mount statistics from withdrawn 0.6.2.

Recent attach never takes over an attached session or recreates one that ended.
Explicit server and SSH options override saved defaults. Typed passwords are not
saved. Default notification mounts require a matching remote sshore and external
OpenSSH sftp-server; --no-watch retains plain SFTP mode.
Client and remote releases must match exactly. Existing-session updates still
require reviewing affected tasks and typing update. Linux/Windows amd64 only.
NOTES
fi

if [[ "$version" == "0.6.4" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Share 154 automatic Pokemon names across shells, forwards, mounts and pending reviews in each local account namespace; retain explicit names and existing IDs.
- Use review/NAME for pending requests; approved forwards retain the action name as forward/NAME.
- Make list an offline, host-grouped view of local connections, resources and timestamped shell history. Record acknowledged detach immediately and mark unconfirmed loss explicitly.
- Make list HOST announce and query only the remote account; --details adds process trees and diagnostic state. Other managers' resources remain separately labelled and read-only.
- Use shell for persistent PTY shells and session for managed running tasks. JSON inventory uses shells instead of sessions.
- Retain only the newest published patch release in each x.y series after verifying replacement assets. Older release assets are removed; Git tags, drafts and prereleases are retained.

Client and remote releases must match exactly. Updates to older shells require
reviewing affected tasks and typing update. Existing names are not migrated.
Pinned downloads of removed patches will no longer be available; use the latest
patch in that x.y series. Linux and Windows amd64 only.
NOTES
fi

if [[ "$version" == "0.6.5" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add Ctrl+B session creation, switching, selection, detach, redraw and help; preserve bracketed paste and literal prefix input.
- Restore the original session when a switch fails before interactive readiness; never take over another attachment.
- Harden terminal input cancellation, detach acknowledgement and failed attachment cleanup.
- Extract native terminal and mux infrastructure, including the Windows ConPTY foundation. Windows local mux CLI remains unfinished; remote shell hosting remains Linux-only.
- Organize copy, mount, review and shared transport code by responsibility, and align documentation with the implementation.

Client and remote releases must match exactly. Updating older shells requires
reviewing affected tasks and typing update, which ends those shells and tasks.
Linux and Windows amd64 only.
NOTES
fi

if [[ "$version" == "0.6.8" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Prepare the exact remote release automatically for notification-enabled mounts, with foreground update confirmation and no persistent shell creation.
- Fall back from remote downloads to checksum-verified client downloads/cache, transferring the executable over the same authenticated SSH connection; keep no-downgrade and explicit update safeguards.
- Fix nested Windows ConPTY keyboard-mode forwarding that turned Alacritty arrow keys into text; clear inherited Win32 input mode.
- Mark the current local shell as attached (current), and separate entry, exit and detach notices from shell output.
- Consolidate copy and mount implementations in a flat internal/files package, share frame encoding, and simplify resource stop coordination.

Client and server require the exact full 0.6.8 release. Updating older remote
shells ends the listed shells and tasks only after typed confirmation.
NOTES
fi

if [[ "$version" == "0.6.7" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add `sshore spawn` for local and remote temporary shells bound to the original parent shell. Detach/reattach keeps that ownership; owner exit ends the spawn and its identified tasks. Remote lifetime-connection loss also ends the spawn.
- Rename the persistent-shell command to `sshore launch`. The existing task column distinguishes `launch/NAME` and `spawn/NAME`, with stable numeric IDs for both.
- Ensure Windows terminal closure ends its process tree even without an output reader.
- Honor `SHELL` on Windows before falling back to `COMSPEC` or CMD, including Nushell executable paths.
- Retry transient installer downloads and report preparation failures explicitly instead of a generic setup EOF.

Client and server require the exact full 0.6.7 release. Updating older remote
shells ends the listed shells and tasks only after typed confirmation.
NOTES
fi

if [[ "$version" == "0.6.6" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Replace `new` with `shell`; create local Linux PTY or Windows ConPTY shells without an SSH server, or remote Linux shells with HOST. Use --detach to create without attaching.
- Share shell daemons, preserve shells after terminal closure or connection loss, and keep single-shell cancellation isolated from sibling shells.
- Let the local daemon own forward SSH processes directly; retain independent mount workers and idle daemon shutdown.
- Add stable numeric task IDs and a unified shell/mount/forward/review table. Remote shell/attach connections show other background tasks through the existing connection.
- Add `cancel HOST --all` and local `cancel --all`, with a displayed snapshot, explicit confirmation and identity checks that protect replacement tasks.
- Fix Windows shell startup, detach input handling and prompt redraw on reattach. Use cmd.exe for local Windows shells.
- Remove Ctrl-] handling; keep Ctrl+B shortcuts. Rename list --details to --verbose, without a -v alias.

CLI migration: use shell instead of new and list --verbose instead of --details.
Connection loss now preserves shells; use exit or cancel to end them. Screens
and detached output are not replayed. Mounts and forwards remain independent of
shell lifetime. Bulk cancellation may end running programs and lose unsaved work.

Client and remote releases must match exactly. Updating older shells requires
reviewing the affected tasks and typing update, which ends those shells and tasks.
Linux and Windows amd64 only; remote shell hosting remains Linux-only.
NOTES
fi

if [[ "$version" == "0.6.9" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Separate forwarding, approval storage/validation and shell attachment protocols into internal business packages; retain CLI and release policy in the main program.
- Name shared shell mechanisms internal/shell and consolidate line editing and path completion under terminal UI infrastructure.
- Bound local shell inventory queries, fix duplicate peer rows and preserve process-tree formatting and receipt expiry.
- Fix Linux shell exit draining, handle-based mount chmod, failed detach notifications, interrupted release downloads and approval cleanup working directories.
- Harden Windows filename and inherited shell environment handling.
- Increase mount stream windows and directory prefetch concurrency; extend metadata/directory cache TTLs to 2/5 seconds while retaining mutation and notification invalidation.

Client and remote releases must match exactly. Updating older remote shells ends
the listed shells and tasks only after typed update confirmation. Existing mounts
need restarting to use the new tuning. External metadata changes can remain cached
longer when notifications are unavailable. Linux and Windows amd64 only.
NOTES
fi

if [[ "$version" == "0.6.10" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Keep spawn ownership fixed across detach and reattachment. Remote spawns retain their background lifetime heartbeat; launch shells remain independent of client lifetime.
- Show lifetime policy in list and JSON, and distinguish original spawn owner contexts from attachment sources in pstree. Reconcile live shell state with cached relationships by instance identity.
- Start supported Linux shells as interactive login shells by default; add --no-login and --no-profile startup controls, with platform-specific Windows handling.
- Group session listings by execution host and consolidate CLI routing, private state, shell startup, transport, terminal, file and review modules and their tests.
- Preserve display-only relationship metadata separately from OS process identity checks and cancellation authority.

Client and remote releases must match exactly. Updating older remote shells ends
listed shells and tasks only after you review them and type update. Detach does
not change lifetime policy: spawns end with their original owner, and remote
spawns also end after lifetime connection loss is detected. Launch shells survive
client exit and connection loss. Login startup may read additional shell startup
files; use --no-login for ordinary interactive startup. Linux and Windows amd64
only; remote shell hosting remains Linux-only.
NOTES
fi

if [[ "$version" == "0.6.11" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Support mount DIRECTORY origin: requests from remote shells through local review, with mountpoint selection, optional --read-only and original-shell validation on the mount connection.
- Add Bash, Zsh, Fish and PowerShell completion from shared command definitions, using local hints without SSH, daemon probes or state mutation.
- Simplify list with host-level or per-row CHECKED times, subdued terminal colors and lifetime details under --verbose; preserve machine-readable JSON fields.
- Present pstree with HOST roots, compact state labels, cross-host suffixes and current-shell highlighting while retaining spawn ownership.
- Hide confirmed ended shells from default list and pstree, reconcile exit/cancel observations with stored relationships, and expire ended list history after seven days.
- Let cancel ID and cancel HOST NAME clear confirmed exited/not-listed history locally without SSH or signals; prevent stale IDs from clearing replacement instances.
- Consolidate command parsing and adapters, and keep completion candidates separate from execution authority.

Client and remote releases must match exactly. Updating older remote shells ends
listed shells and tasks only after you review them and type update. Offline list
does not probe remote hosts; run list HOST to refresh that account. Historical
connection shortcuts no longer create task rows. Use completion --help for shell
registration and list --help for display/history behavior. Linux and Windows
amd64 only; remote shell hosting remains Linux-only.
NOTES
fi

if [[ "$version" == "0.6.12" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Pipeline copy transfers and verify uploaded content on the receiver before no-overwrite publication, avoiding a full network readback. Direct cp uses the shared exact-version remote setup flow.
- Preserve unrelated mount metadata during local mutations and actively verify cached metadata in the background, prioritizing recently accessed files.
- Bound and coalesce background verification with debounce, rate limits, transport-failure backoff and cancellation; foreground reads never wait for the verification queue.
- Reuse authenticated daemon health responses during upgrade inspection instead of opening a second status connection.
- Consolidate command policy and platform adapters by ownership, and move black-box integration coverage into tests/ with isolated lifecycle binaries.

Client and remote releases must match exactly. Updating older remote shells ends
listed shells and tasks only after you review them and type update. Direct cp now
prepares the matching remote sshore executable. Restart existing mounts to use the
new cache behavior. Alias metadata may remain temporarily stale until background
verification, notification or cache expiry; no failed write is replayed. Linux and
Windows amd64 only; remote shell hosting remains Linux-only.
NOTES
fi

release=$(gh release list --limit 1000 --json tagName --jq ".[] | select(.tagName == \"$tag\") | .tagName")
if [[ -n "$release" ]]; then
  echo "Release $tag already exists; keeping its assets and checking retention."
else
  gh release create "$tag" --verify-tag --title "sshore $tag" \
    --notes-file release-notes.md --latest \
    packages/*.tar.gz packages/*.zip packages/SHA256SUMS
fi

# Fetch every page before planning deletions. Failed publication or incomplete
# replacement assets must never remove a working release. Tags are untouched.
retention_dir=$(mktemp -d)
trap 'rm -rf "$retention_dir"' EXIT
gh api --paginate --slurp "repos/$GH_REPO/releases?per_page=100" > "$retention_dir/releases.json"
python3 "$(dirname "$0")/prune_releases.py" "$tag" < "$retention_dir/releases.json" > "$retention_dir/remove.tsv"
while IFS=$'\t' read -r release_id old_tag; do
  echo "Removing superseded release $old_tag (keeping its Git tag)."
  gh api --method DELETE "repos/$GH_REPO/releases/$release_id"
done < "$retention_dir/remove.tsv"
