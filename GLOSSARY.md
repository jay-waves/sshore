# Terms and relationships

This document defines the project vocabulary. Commands are documented by
`sshore COMMAND --help`; implementation and security rules belong in
[SSHORE.md](SSHORE.md) and [SECURITY.md](SECURITY.md).

## Machines and connections

| Term | Meaning |
| --- | --- |
| Local computer | The machine running the client or managing a mount/forward. Local is relative to that operation. |
| Remote host / SSH destination | The host reached through system OpenSSH, often named by a `~/.ssh/config` alias. |
| Remote account | The authenticated Unix account. Session management excludes other users. |
| Client | The local sshore process connecting to a remote session. It is not the session itself. |
| Transport | The SSH connection carrying a shell attachment, independent forward or SFTP mount. |
| Attachment | One client's live connection to a persistent session. At most one client is attached at a time. |
| Peer / manager | Another computer managing a resource whose advisory record is visible on the remote account. |

## Sessions and lifecycle

| Term | Meaning |
| --- | --- |
| Mux | The detach/reattach capability: explicitly leave a remote shell running and attach again later. The internal smux library separately multiplexes transport streams. |
| Session | A remote persistent shell and PTY owned by a session daemon. It can survive explicit detach. |
| Daemon | The remote process owning a session's PTY, helper endpoint and lifecycle. |
| Session name | A public label, automatic or user-supplied; renaming it does not replace the daemon or shell. |
| Task | A program associated with a session. Identifying tasks for termination requires process identity checks, not just a displayed PID. |
| Detach | Explicitly end an attachment while preserving its session. Ctrl-] and `sshore detach` request this. |
| Disconnect | Loss of transport. For an attached shell, it ends the session once detected unless explicit detach already occurred. |
| Cancel | The public action for discarding a request, stopping a resource or ending a named remote session. Its scope depends on the identifier. |
| Recovery | A temporary shell on the authenticated SSH connection, without persistent session or normal sshore operations. It is not a sandbox. |
| Release version | Full MAJOR.MINOR.PATCH identifier used for compatibility. There is no independent sshore protocol version counter. |

## Requests, resources and views

| Term | Meaning |
| --- | --- |
| Request | Bounded metadata proposing a file operation or forward; receiving it grants no permission. |
| Pending approval | A local single-use request record, identified as `approval/ID`, awaiting user review. |
| Review | The separate local command that selects a pending request, collects consent and attempts execution. |
| Consent / approval | The user's explicit acceptance in review. SSH authentication alone is not consent. |
| SCP use case | The README category for one-time file copies. sshore uses its own transfer protocol, not SCP wire compatibility or `scp` command compatibility. |
| Tunnel | The README feature category for TCP port forwarding through SSH. |
| Transfer | An approved one-time regular-file upload or download using sshore's framed protocol. It is not SFTP. |
| Resource | An independently managed local mount or forward, with its own worker, transport and lifetime. |
| Worker | The local background process owning a resource; distinct from a remote session daemon. |
| Mount | A remote directory exposed to local applications through SFTP and FUSE/WinFsp, identified as `mount/NAME`. |
| Forward | A fixed TCP port mapping through SSH, identified as `forward/NAME`. It can carry an existing SOCKS service; it does not itself implement SOCKS. |
| Native forwarding | OpenSSH `-L/-R/-D` configured for a connection, separate from a managed `forward` resource. `-D` creates a SOCKS proxy. |
| Inventory | A diagnostic view of resources, requests, sessions or peer records. It is not an authorization source. |
| Peer record | A remote advisory mirror, identified as `peer/KEY/RESOURCE`; it cannot control the original resource. |
| Receipt | A bounded diagnostic record of a session/resource ending. It is not proof of complete task cleanup or remote write outcome. |

## Relationships and scope

- A **client** attaches through a **transport** to a **session** owned by a remote
  **daemon**. Detach removes the attachment, not the session.
- A remote helper creates a **request**. Local **review** obtains consent and checks
  its source session before attempting a transfer or starting a forward.
- A **transfer** belongs to its executing connection. An approved **forward** becomes
  an independent resource and leaves the source shell detached.
- A **mount** is created explicitly on its managing computer; it is not a remote
  approval request. Ending a shell does not stop its independent resources.
- `cancel approval/ID` discards a request; `cancel mount/NAME` or
  `cancel forward/NAME` stops that local resource; `cancel HOST NAME` ends that
  remote session and its identified tasks. A peer record is read-only.
- In `CURRENT_PORT to PEER_PORT`, the listener is on the invoking shell's machine.
  In `CURRENT_PORT from PEER_PORT`, the listener is on the other machine. These
  directions are resolved to fixed client/server endpoints before approval.
- **Worker running**, **transport connected** and **target healthy** are different
  claims. Listings do not probe target services; missing peer heartbeats mean
  unknown state, not confirmed termination.

The legacy `ftp` filename prefix does not name a supported command or plain FTP
transport. The command is `mount`; its protocol is SFTP over SSH. `review` replaces
`approve`, and `cancel` replaces the public `stop`/`kill` verbs. Internal wire or
function names may retain historical terminology.
