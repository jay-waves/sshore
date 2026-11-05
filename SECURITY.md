# Security model

sshore trusts the **local OS, terminal, user account, executable and SSH
configuration**. The remote host may be compromised, including its root user,
session daemon and programs running as your remote account. SSH authenticates
the endpoint and protects transport; it does not make that endpoint trustworthy.

## Approval boundary

```mermaid
sequenceDiagram
    participant R as Remote program / daemon
    participant C as Local sshore client
    participant U as User in local shell
    participant A as Local sshore approve
    R->>C: Structured file/port request
    C->>C: Validate, save expiring pending record
    C-->>R: Queued acknowledgement
    C->>R: Detach and close owned SSH transport
    Note over R: Shell and programs continue running
    C-->>U: Client exits; local shell resumes
    U->>A: approve [ID] --attach or --stay
    A->>U: Show target, operation and local paths
    U->>A: Explicit approval
    A->>A: Consume request once
    A->>R: New system SSH connection; initiate exact operation
    Note over A,R: OpenSSH owns keys, agent, authentication and host checks
    A-->>U: Attach, or stay local (foreground for a port tunnel)
```

For a single upload, submitting the local source path after reviewing the host
and remote destination is explicit approval; there is no additional `yes`. Other
requests require a final `yes` after path selection. Path completion only browses
the local filesystem during this separately invoked approval command.

Receiving a request never opens a requested local listener or reads/writes a
user-selected transfer file. Only the bounded pending metadata is saved.
Unsolicited file offers, upload requests and forward-add streams are rejected.
`--download-dir` supplies a default, not consent. Requests cannot encode a local
shell command or select an upload source.

Pending records are private to the local user, expire in 10 minutes, and are
consumed before reconnecting—even on failure. The original connection options,
session and shell PID are retained; approval cannot silently create a new shell.
PID matching catches accidental session replacement, not a malicious server
lying about its identity. Local SSH configuration and executable selection remain
trusted and may affect reconnects. ControlMaster sharing is disabled for sshore's
own connection; configured jump hosts/proxies may have their own lifetimes.

## Attacks considered

| Attacker behavior | Protection / limit |
| --- | --- |
| Send unsolicited files or bind local ports | Local execution requires a separately invoked approval command; remote confirmation is never trusted. |
| Traverse download paths or overwrite files | Validate basename; user selects local destination; temporary file plus atomic hard link prevents overwrites. |
| Change an operation after approval | Local snapshot fixes paths, direction, name/size and port. Changed file metadata is rejected. Same-size content changes remain possible. |
| Send excess data or replay an approval | Framing and per-stream buffers are bounded; received bytes cannot exceed the approved size; local record is single-use. |
| Flood requests | At most 16 operations per request and 32 pending records; the first accepted request detaches. Repeated disruption across reconnects remains possible. |
| Impersonate a local service | Approved forwards bind only `127.0.0.1`, fail on occupied ports and close on disconnect. A forwarded service is still remote-controlled; loopback is not per-user isolation. |
| Forge terminal prompts | Approval starts from a separate local invocation after client exit. Remote terminal output is still untrusted and can imitate text; do not run approval or enter credentials merely because remote output asks. |

## Remaining limits

- Approval is consent, not a malware scan. Downloaded content and forwarded
  services may be malicious. SHA-256 checks transfer integrity, not provenance.
  Use a dedicated download directory; avoid directories that automatically load
  new scripts/configuration. An approved large file can still fill your disk.
- No separate approval password is collected. System SSH may authenticate using
  an agent/key without prompting. Successful SSH login is not the approval gate.
- Remote processes sharing your remote account (and remote root) can request
  operations and disrupt sessions. Requests do not prove who typed the command.
- A compromised local account/root, terminal emulator vulnerabilities and users
  approving deceptive requests are outside this boundary. Approval records are
  not tamper-proof against their owner. Terminal output is not sandboxed.
- Local `-L`, `-R`, `-D`, agent forwarding and other SSH configuration are explicit
  local policy, outside remote-request approval. They are reused on reconnect;
  in particular, `-R` can expose a local service to the remote host.
- `--stay` on a port keeps a foreground connection, not a detached background
  service. Ctrl-C closes it. Detach preserves the remote shell, but cancels
  connection-owned transfers/forwards. Forced termination may leave temporary
  `.sshore-*.part` files. Batch failure does not roll back completed files.

Use exact full versions on both ends. Managed setup scans only the current
remote account's visible session daemons. A newer remote version rejects an older
client; no automatic downgrade is performed. Do not bypass SSH host-key checks.

## Remote setup and recovery

Missing or older remote software may trigger HTTPS preparation of a matching
executable through `install.sh`. The installer checks SHA256SUMS and the binary
version in a temporary directory before setup changes the installed executable.
Existing installations or old sessions require typing `update` at the local
setup prompt before cleanup. Setup displays affected session identities, locks
managed setup operations per account, rechecks the snapshot, and signals only
identified same-user tasks with PID identity protection. Updates may interrupt
other clients sharing that account and lose unsaved work. Unknown versions and
unsupported signaling/permissions require manual handling. Processes that escape
the session ancestry/session ID, legacy binaries ignoring this policy, and
concurrent unmanaged modifications are outside this guarantee.

Choosing manual recovery preserves the SSH connection and starts a transient PTY
shell. Inherited helper socket variables are removed and new sshore binaries
honor `SSHORE_DISABLED=1`. No approved transfer is executed in recovery or after a
setup update. This is not a security sandbox or a restriction on arbitrary remote
executables. Explicit OpenSSH forwarding remains the local user's policy.

Installer downloads require remote HTTPS access and a matching release asset.
If preparation fails, no old session is terminated. Auto replacement is restricted
to a user-owned regular file and non-group/world-writable user-owned directory;
there is no elevation, PATH or shell-profile modification. A checksum detects
corruption, not compromise of the publisher/repository supplying both the script
and checksums. Standalone Windows installation does not stop processes or create
versioned backups. Published releases and assets are retained.
