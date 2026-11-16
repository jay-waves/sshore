# Security model

sshore separates remote shell interaction from permission to access local files
and services. **An authenticated remote host is not necessarily trustworthy.**
Remote requests are proposals; only an explicit local command or local review
grants access.

This document describes threats, controls and their limits. Implementation and
lifecycle details belong in [SSHORE.md](SSHORE.md); terminology belongs in
[GLOSSARY.md](GLOSSARY.md).

## Scope and assumptions

The model protects local files and services from unapproved remote requests,
approvals from replay or substitution, and managed tasks from accidental takeover
or termination of the wrong process. It also limits what state and diagnostics
expose. It does not promise availability or trustworthy remote computation.

| Actor or component | Assumption |
| --- | --- |
| Local OS, account, terminal, sshore executable and SSH configuration | Trusted. Administrators and processes running as the same local user are inside this boundary. |
| Remote host, daemon and programs in the remote account | Potentially malicious. They may forge requests, metadata, terminal output and responses. |
| Network | Untrusted. System OpenSSH provides endpoint authentication and transport integrity under the user's host-key policy. |
| Release publisher and distribution repository | Trusted to supply the installer, executable and checksums. Checksums alone do not protect against a compromised publisher. |
| Local user | Chooses commands and reviews requested access. Approval is not a malware scan or proof that the remote requester is honest. |

A compromised remote root or kernel can ignore lifecycle rules, lie about files
and process identities, or retain data. Remote-side checks protect against mistakes
and ordinary races on a cooperating host; they cannot compel a hostile host.

## Trust boundaries

```mermaid
flowchart LR
    subgraph remote["Potentially malicious remote host"]
        shell["Shell output and operation requests"]
        service["Files and services"]
        setup["Remote setup and managed shells"]
    end
    subgraph local["Trusted local OS and user account"]
        client["Connected client"]
        pending["Bounded, private, expiring request"]
        user["Local user"]
        review["Separate local review: select paths, mountpoint or ports"]
        grant["Fixed operation; consume approval once"]
        resources["Selected files, tunnel or mount"]
        control["Local daemon / workers: private capability + exact version"]
        update["Review affected shells; type update; recheck identities"]
    end
    shell -->|"Authenticated SSH; content remains untrusted"| client
    client -->|"Save metadata and detach; no operation yet"| pending
    pending --> review
    user --> review
    review --> grant
    grant -->|"Revalidate original shell; reject changed setup / recovery"| resources
    user -->|"Explicit cp / forward / mount command"| resources
    resources <-->|"Authenticated SSH; remote account permissions"| service
    user -->|"Local shell and resource control"| control
    resources -->|"cp / watched mount: exact-version helper setup"| update
    user --> update
    update -->|"Prepare verified replacement before ending confirmed tasks"| setup
```

Remote output, public names, numeric task IDs and inventory records are not
capabilities. Local control requires private state and authentication. Remote
operations remain constrained by the authenticated account and SSH server policy.

## Threats and safeguards

| Attack scenario | Primary gate | Remaining risk |
| --- | --- | --- |
| Remote output tricks the user into granting local access | [Separate local review](#local-approval) | A user may still approve a deceptive request. |
| An old request is replayed or redirected to another shell | Single-use records and original-shell checks | A malicious server can lie about its own identity. |
| A file request overwrites a local file or changes after approval | [Selected paths and no-overwrite publication](#files-and-forwarding) | Content can be malicious or change without changing size. |
| A remote program exposes a local service | Locally selected, fixed forwarding endpoints | Loopback is not per-user isolation; server policy can alter reverse binds. |
| Another process takes over or cancels unrelated work | [Private capabilities and process identity checks](#shell-control-and-lifetime) | Same-user compromise and deliberately escaped tasks are outside the guarantee. |
| Setup silently downgrades software or kills tasks before replacement is ready | [Exact versions and typed update confirmation](#remote-setup-and-updates) | Confirmed termination cannot be undone. |
| A remote filesystem hangs, floods responses or leaves writes uncertain | [Bounded mount requests and no write replay](#sftp-mounts) | Partial writes, stale data and resource exhaustion remain possible. |
| Forged status records become authority to act | [Advisory inventory only](#inventory-and-privacy) | Displayed status may be false or stale. |

## Local approval

**Threat:** a remote program asks the connected client to read a local file,
write a destination, expose a mount, open a listener or reach a local service without consent.

Receiving a request saves bounded metadata and detaches. It does not execute a
local shell command, create a mount, open a listener or transfer a selected file. Unsolicited
transfer offers, forward-add streams and reverse connection streams are rejected.
A suggested path or port, including `--download-dir`, is not permission.

Sensitive review happens in a **separate local `sshore review` invocation** after
the connected client exits. Submitting the validated paths or port is approval;
there is no additional `yes`. File batches collect all paths before starting.
Ctrl-C or EOF during selection starts no operation and leaves the request pending.
Completion can browse local paths but does not submit them. Remote terminal output
is suppressed during approved operations and `--stay`.

Each pending request:

- Is private to the local user, expires after ten minutes and is subject to
  request/operation limits.
- Stores connection options and operation metadata, not collected SSH credentials.
- Is consumed before reconnecting, even if connection or execution fails. A normal
  shell connection cannot execute it; there is no persisted grant or automatic retry.
- Retains its original shell identity and PID. Review does not silently create a
  replacement shell. Rename or replacement can invalidate a pending request.
- Fixes the approved operation locally. Setup changes or recovery skip execution;
  the user must request the operation again.

OpenSSH owns passwords, keys, agents, MFA and host-key checks. Logging in without
an authentication prompt is not approval. Local SSH configuration and executable
selection are trusted; configured proxies and jump hosts may have independent
lifetimes. sshore disables ControlMaster sharing on its own transports.

**Limit:** remote output can imitate local prompts, and processes sharing the
remote account can submit requests. A request does not prove who typed it. Do not
enter credentials or invoke review solely because remote output tells you to.
Private records are not tamper-proof against their owner.

## Files and forwarding

### File substitution, overwrite and malicious content

Direct copies are explicit local grants. Approved copies expose only the selected
file and temporary upload, not a general remote filesystem. Both paths use bounded
transfers, temporary files and atomic hard-link publication without overwriting;
there is no overwrite-prone fallback. Approved file sizes bound received bytes.
Changed metadata is rejected, and uploads verify stored temporary content by
size and SHA-256 readback on the receiving sshore before publication. The sender
provides its digest; the receiver closes writes, verifies its temporary file and
permits publication only on a match. File content is not downloaded for verification.

Local sources reject final-component symlinks/reparse points and compare opened
file identity with the initial check. Linux uses nonblocking opens so a substituted
FIFO cannot stall validation. These checks do not lock parent directories or stop
changes to the contents of the same file. Same-size content changes remain possible.

SSH integrity and readback checks do not authenticate file provenance. Downloaded
content can be malicious; avoid destinations that automatically load scripts or
configuration. Metadata is not copied. Received Linux files default to `0600`;
Windows files inherit the destination directory ACL.

Direct copies also permit explicitly selected stdin/stdout. Stdin can be unbounded
and fill remote storage; stdout cannot retract bytes after failure. Completed files
remain when a batch fails. Cancellation removes temporary files when cleanup can
run, but forced termination can leave `.sshore-*.part` files. Connection loss
cancels connection-owned transfers; it does not undo completed operations.

### Unwanted listeners and local service access

Forwarding fixes direction, listener and destination before execution. Independent
forwards request IPv4 loopback listeners and loopback destinations; occupied ports
fail without substitution. Remote requests need local port selection or acceptance.
Their helper rechecks the original shell version, name, PID and detached state.
No software update runs during forward review. OpenSSH may bind an approved
listener before that check; failure closes the transport before it reports ready.

**Limits:** loopback is not per-user isolation. SSH server `GatewayPorts` policy
can override reverse listener binding, and forwarded services may be malicious.
Local `--retries` explicitly permits bounded reconnection of the same mapping;
it is unavailable to remote requests. Retries require noninteractive authentication
and known host keys, never restore existing application connections, and stop on
cancellation.

Native SSH forwarding (`-L`, `-R`, `-D`), agent forwarding and SSH configuration are
separate local policy, outside remote-request approval. `-R` can expose a local
service to the remote host. Do not bypass host-key checks.

## Shell control and lifetime

**Threat:** a stale name, reused PID or forged status row causes takeover or
termination of unrelated work.

Local daemons and shell endpoints listen on loopback and require random capabilities
from current-user private state. Unix ownership/permissions and Windows DACLs
protect that state. Exact release checks and lifetime locks prevent silent takeover
or downgrade. Launch environments remain in memory, not public inventory or
persistent daemon records. This is a same-user trust boundary, not a sandbox.

`launch` reserves a fresh name; `attach` requires an existing shell and never evicts
its attached client. Selection is rechecked before attachment. Recent targets only
supply defaults: they do not carry typed passwords, authorize takeover or recreate
ended shells. Rename changes a label, not process identity or approval authority.

Lifecycle is fixed at startup:

- `spawn` retains the original parent PID and creation identity, even after detach
  or attachment elsewhere. PID reuse does not extend its lifetime.
- Remote spawn additionally requires an exclusive lifetime SSH lease on its exact
  endpoint. Detach preserves that lease; owner exit or detected connection loss
  ends the spawn. Silent network loss is detected after a timeout.
- `launch` survives client exit and connection loss. Exit, explicit cancellation,
  daemon failure or host restart can end it. Independent mounts and forwards do
  not end merely because a shell ends.

Explicit cancellation validates the target instance, capability or process identity;
it never kills a PID solely because a record contains it. Remote shell cancellation
requires matching client/program/daemon releases, the account setup lock, a same-user
socket peer and confirmed process identities. It does not install or update software.
Single-shell cancel can end an attached shell immediately. `cancel --all` displays a
fresh snapshot and requires typing `cancel`; new tasks, same-name replacements and
other managers' advertisements are excluded.

Linux managed cleanup signals only confirmed same-user shells and identified tasks.
Windows spawns use kill-on-close jobs. Ordinary PTY closure relies on terminal hangup
semantics: programs ignoring hangup or deliberately escaping/reparenting may survive.
No process-name-wide kill is used. A daemon failure can affect all terminals it owns.

`pstree` owner contexts and attachment sources are display metadata, not verified
OS ancestry. They never authorize signaling or change the actual owner or lease.

## SFTP mounts

**Threat:** a remote filesystem gains unintended local authority, returns hostile
paths/data, or leaves an application waiting indefinitely on uncertain writes.

A local `mount` command explicitly grants access to a remote directory. Applications
with access to that mount can read and, unless `--read-only` is set, overwrite,
rename or delete files using the remote account's permissions. A remote program
may request a mount through the [local approval gate](#local-approval), but cannot
approve it or choose the final local mountpoint. Review displays the access mode;
startup revalidates the original detached shell on the mount's SSH connection.
The resulting mount is independent of that shell. Recovery mode rejects it.

The selected root is a navigation boundary, **not remote-account confinement**.
Remote symlinks can be followed with the account's permissions. Linux readlink
results are restricted to the mounted tree so absolute remote links cannot redirect
local tools into unrelated local files. Windows rejects unrepresentable names
instead of silently aliasing them. Remote filenames, attributes and data remain
untrusted.

Notification-enabled mounts use the same [setup safeguards](#remote-setup-and-updates)
as shells before starting an exact-release transient agent. Declining setup does not
grant recovery-mode file access. No-watch mounts use ordinary SFTP without remote
sshore. The agent needs remote command execution and does not inherit an
internal-sftp-only policy or SFTP subsystem-specific command flags. SSH/account
policy remains authoritative; the mount connection disables native forwarding,
backgrounding and ControlMaster sharing.

Outstanding SFTP requests have individual deadlines that other responses or
keepalives cannot extend. Timeout aborts the connection; **a write may already have
completed remotely**. No write is replayed. There is no persistent write-back spool,
conflict detection, local-disk locking or atomic multi-operation transaction.
A server response is not proof that a compromised server stored data.

Normal shutdown tries to unmount while the transport is live; a busy mount stays
connected for retry. Forced shutdown may leave partial writes. Close applications
before stopping a mount. Exclusive creation prevents overwriting a new file, but a
failed permissions operation can leave an empty remote file.

Packets, directory responses, caches and watch subscriptions have bounded budgets.
Oversized listings fail rather than silently truncate. Caches and open directory
snapshots can show stale state; notifications are advisory and may be lost.
Local mutations invalidate known paths; hard-link or symlink aliases and paths of
renamed open files may remain stale until background verification, notification
or cache expiry. Background verification is bounded and advisory; it cannot
prove file identity or authorize an operation. Neither
notifications nor status queries authorize file operations or write replay. See
[development guidance and code ownership](SSHORE.md#shared-local-daemon-and-resource-workers).

## Remote setup and updates

**Threat:** setup runs an unintended executable, silently downgrades software,
or ends tasks without informed consent or a ready replacement.

The full release version, including PATCH, must match. Managed scope is the current
remote Unix account's visible sshore shell daemons across runtime directories;
other users, preparation helpers and transient transports are excluded.

1. Missing software may be installed automatically when no incompatible shells
   exist. Existing older installations or shells require reviewing affected
   identities and typing `update`. Newer versions reject older clients; unknown
   or unordered versions permit manual recovery only.
2. Preparation selects the exact release, verifies its checksum and executable
   version, and stages it without replacing the installed program. Client-side
   download fallback uses the same authenticated SSH connection; the remote cannot
   choose a local path, URL or different release. Extraction accepts only the
   bounded regular executable.
3. Before ending tasks, setup takes the account lock, rechecks the snapshot and
   prepares/verifies the replacement on the target filesystem. Changed state
   requires fresh confirmation. Preparation failure must not terminate tasks.
4. Only confirmed same-user shells and identified tasks are signaled, with process
   identity checks. Once termination starts, later failures cannot restore stopped
   tasks. Updates can disconnect other clients and lose unsaved work.

Automatic replacement requires a user-owned regular file and a user-owned directory
without group/world write permission. It does not elevate privileges or modify PATH
or shell profiles. Direct copy and notification-enabled mount startup use the same
safeguards and collect any confirmation in the initiating terminal. No-watch mounts
and ordinary forwarding do not install a remote helper.

Release checksums detect corruption, not publisher compromise. Cached artifacts are
private and rechecked on reuse; unavailable assets never cause substitution of a
different release. Standalone installers do not stop running shells. Published tags
and assets are not overwritten; older patch assets are removed only after a complete
replacement is published. See [release mechanics](SSHORE.md#release).

## Manual recovery

Recovery preserves the authenticated SSH connection and provides a transient PTY
shell. Persistence, approval execution, file transfer, dynamic forwarding and local
Ctrl+B shortcuts are disabled; help, version and diagnostics remain available.
Normal functionality requires reconnecting. Approved operations are skipped after
recovery or an update and must be requested again.

Inherited helper socket variables are removed; new binaries honor
`SSHORE_DISABLED=1`. **Recovery is a product mode, not a sandbox:** arbitrary legacy
binaries and deliberately escaped/reparented tasks are not globally constrained.
Explicit native SSH forwarding remains the local user's policy.

## Inventory and privacy

Shell completion reads bounded local records, cached tasks and literal SSH aliases.
It does not execute SSH configuration commands, contact hosts or daemons, create
state or allocate IDs. Candidate text remains untrusted data and is quoted by shell
adapters. Only static registration code is sourced; cached suggestions never bypass
normal execution checks. Recovery produces no completion candidates.

Public inventory is an allowlist: it excludes control addresses/tokens, SSH options
and terminal contents. Shell process listings show sanitized same-user names and
PIDs, not arguments or environments. Foreground summaries and detailed resource
status can reveal working-directory/project names, paths, ports and errors.

Peer records expose only their resource identity, manager label, remote-facing
path or port direction, and state. They exclude local mount paths and unrelated
hosts. A remote account can forge its own inventory. Records expire, can become
unknown and are read-only; removing a record never signals a process.

Cached shell relationships, numeric task IDs, exit receipts and worker liveness
are not proof of identity, successful cleanup or remote service health. Targets
are not probed. Detailed remote inventory checks the exact release but never
installs or updates software.

Private exit records are bounded and expire. Shell receipts contain name, time and
reason, not terminal content, arguments or working directories; resource receipts
can contain paths and errors. Abrupt termination can prevent cleanup or recording.
See [inventory and diagnostics](SSHORE.md#inventory-and-diagnostics) for implementation entry points.

## Remaining risks

- Local account/root compromise, terminal-emulator vulnerabilities or deliberate
  user approval of a deceptive request defeats this boundary. Displayed paths and
  errors are sanitized, but raw remote terminal output remains untrusted; terminal
  compatibility filters are not a security boundary.
- Limits bound individual requests and buffers, not all resource consumption.
  Approved large files or stdin streams can fill disks. Repeated requests can disrupt
  work, and a hostile host can delay, disconnect or falsify responses.
- Process visibility can limit cleanup. Deliberately escaped tasks, legacy binaries,
  unmanaged concurrent changes and a hostile remote kernel are outside termination
  guarantees. Diagnostics never authorize termination by themselves.
- SSH authenticates a transport endpoint. It does not authenticate file provenance,
  make a forwarded service safe, or guarantee the remote host's behavior.

Host grouping identities use MD5 solely as compact display metadata, never as authentication or integrity evidence. A new connection entry point for a known identity produces a remembered advisory warning; it does not block SSH or prove cloning.
