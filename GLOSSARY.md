# Terms and relationships

This document defines the shared vocabulary, not an object hierarchy.
CLI help owns command syntax; [SSHORE.md](SSHORE.md) owns implementation and
[SECURITY.md](SECURITY.md) owns authorization and trust boundaries.

## Tasks

| Term | Meaning |
| --- | --- |
| Task | A daemon-owned item: a terminal shell, mount or forward. One in-memory account table owns its identity and lifecycle; each kind retains its execution mechanism. Programs running inside a shell are its processes, not another sshore task type. |
| Shell / session | A daemon-owned shell process with one terminal. Local shells run on Linux or Windows; remote shells currently run on Linux. A session means a shell session, never a mount or forward. |
| Pin | Retention on a shell. `sh --pin` retains it until exit. `detach [DURATION]` retains the running shell as it releases its terminal. |
| Mount | A remote directory exposed to local applications through SFTP and FUSE/WinFsp. |
| Forward | A fixed TCP port mapping through SSH. It carries TCP traffic, including an existing SOCKS service, but does not implement SOCKS itself. Use this term instead of tunnel. |
| Copy / transfer | A one-time regular-file copy through SFTP, invoked directly or executed after approval. It belongs to its executing connection. |
| Resource | A collective name for mounts and forwards, which have lifetimes independent of shell attachments. It does not introduce another task type. |

## Connections and ownership

| Term | Meaning |
| --- | --- |
| Local / remote | Relative to the operation: local is the computer running the client or managing the resource; remote is its SSH destination. The remote account is the authenticated Unix account, excluding other users. |
| Client | The sshore process interacting with the user. For nested attachments, the client at the user's terminal continues to own input and shortcuts. |
| Daemon | The unique background manager for an OS account, reached through one authenticated loopback TCP endpoint. It owns the in-memory task table, activity leases and current metadata; no daemon-crash recovery is promised. A mount isolation worker is an implementation role, not another manager. |
| Activity lease | A daemon-owned registration retained by a live authenticated connection or an in-process owner. The shared daemon heartbeat detects lost clients. Releasing a lease ends that participation; persistent shell lifetime remains independent. |
| Attachment | A client's live interactive connection to a shell. Each shell tree grants its keyboard to one foreground shell. Its suspended parents are shown as `waiting`. Explicit detach retains the shell before releasing the attachment. Loss of the terminal connection follows the current shell scope. Mux means this detach/reattach capability. |
| Transport | The connection carrying operations, usually SSH; local attachments use local control connections. The authenticated starting lease is separate from the foreground attachment. |
| Owner | The starting client lease or parent shell that supplies a temporary shell's lifetime. Entering an idle root from a shell on the same daemon makes that shell its parent. The account daemon applies pins and cleans up ended tasks. |
| Attachment source | The managed shell from which the current attachment was opened. It may change on reattachment and does not control the target shell's lifetime. |
| Peer | Another computer managing a resource. Its remotely visible peer record is advisory and read-only; it cannot control the original resource. |

Nested attachments retain the initiating host's connection context through a
private route. Switching attachments does not move SSH configuration or signing
credentials to the user's computer. See
[connection ownership](SSHORE.md#shell-ownership-and-attachment).

## Requests and approval

| Term | Meaning |
| --- | --- |
| Request | An immutable proposed copy, mount or forward presented to the local user. Receiving a request grants no permission. |
| Approval | The user's explicit acceptance of a request and local endpoints, after source validation. Consent means the same thing; SSH authentication alone is insufficient. Approval is single-use. |
| Legacy recovery | A temporary shell offered by older releases when setup could not complete. Current setup aborts instead; inherited disabled-mode restrictions remain until reconnecting. |

An approved copy runs on a separate file stream without terminal attachment. An approved mount or forward becomes
an independent resource with its own task UID and name.
Ending a source shell does not stop its independent resources.

`here:` and `.:` identify the machine running the command. `origin:` and `::`
identify the currently attached client. A remote request's path or port is an
editable default in local approval.

## Identity and state

| Term | Meaning |
| --- | --- |
| ID / NAME | A task has one immutable UID generated from 16 cryptographically random bytes and encoded as 32 lowercase hexadecimal characters; the first six characters are its displayed reference. An optional NAME is a readable label set at creation or by `rename`; it identifies the same task. References are scoped to the managing account. An ID selector accepts 2–32 hexadecimal characters and must identify one task; collisions require a longer prefix, the full UID or an explicit scope. |
| Account identity | A private identifier used to group account display records. It is not SSH authentication; copying its state can duplicate it. `reset` replaces it. |

### Task ID

The UID identifies the instance; its prefix is the short form of that same ID.
NAME is optional and does not change the ID or lifetime. Shells, mounts and forwards share the managing computer's name namespace.
The daemon claims the name when it registers the task. It rejects a requested name
already in use. For an unnamed task it uses the shortest available prefix of the UID
starting at six characters.

The selected host resolves task IDs in its live registry. Saved connections
supply SSH settings and routes for reconnecting. A running worker does not prove its SSH connection or target is
healthy. Missing observations mean unknown state, not confirmed termination.
See [inventory and diagnostics](SSHORE.md#inventory-and-diagnostics).
