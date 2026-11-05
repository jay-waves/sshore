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

Use matching protocol-4 binaries on both ends. Exit old session daemons with
the old client before upgrading; do not bypass host-key verification to fix a
connection error.

## User-run online installer

Missing-server detection only prints a command; it never installs software.
`install.sh` (Linux) and `install.ps1` (Windows) run only when invoked by the
user, without elevation or shell-profile changes. They restrict downloads and
redirects to HTTPS, select amd64/arm64 release assets, check SHA256SUMS and the
binary version, and stage replacement in the installation directory. The Windows
installer does not modify PATH or execution policy, stop running executables, or
maintain versioned backups. Users choose a trusted, writable installation
directory and handle backups, permissions and environment configuration.
The script, releases and checksums share the GitHub repository trust boundary:
a checksum detects corruption, not compromise of that repository/publisher.
Piping a script to `sh` or PowerShell `iex` executes repository code; download and inspect it first
if desired. Matching versions and old-daemon upgrade rules still apply.
