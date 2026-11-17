# Terms and relationships

This document defines the shared vocabulary, not an object hierarchy.
CLI help owns command syntax; [SSHORE.md](SSHORE.md) owns implementation and
[SECURITY.md](SECURITY.md) owns authorization and trust boundaries.

## Tasks

| Term | Meaning |
| --- | --- |
| Task | A daemon-owned item: a terminal shell, MCP session, mount, forward or pending request. One in-memory account table owns its identity and lifecycle; each kind retains its execution mechanism. Programs running inside a shell are its processes, not another sshore task type. |
| Shell / session | A daemon-owned shell process with one terminal. Local shells run on Linux or Windows; remote shells run on Linux. A session means a shell session, never a mount or forward. |
| Spawn | A shell created by `spawn`. It ends with its original owner; a remote spawn also ends when its daemon-owned lifetime SSH connection is lost. A shell created by `launch` survives detach and connection loss. |
| MCP session | A persistent remote Bash process controlled by MCP tools, using pipes instead of a terminal. It cannot be attached to interactively. |
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
| Attachment | A client's live interactive connection to a shell. A shell permits one attachment at a time. Detach ends the attachment; disconnect loses its transport. Neither changes the shell's startup lifetime. Mux means this detach/reattach capability. |
| Transport | The connection carrying operations, usually SSH; local attachments use local control connections. A remote spawn's lifetime SSH connection is separate from its foreground attachment. |
| Owner | The original process whose lifetime controls a spawn, verified by process identity. Reattaching never transfers ownership. The managed shell shown as owner in `pstree` is display context, not proof of OS process ancestry. |
| Attachment source | The managed shell from which the current attachment was opened. It may change on reattachment and does not control the target shell's lifetime. |
| Peer | Another computer managing a resource. Its remotely visible peer record is advisory and read-only; it cannot control the original resource. |

Nested attachments retain the initiating host's connection context through a
private route. Switching attachments does not move SSH configuration or signing
credentials to the user's computer. See
[connection ownership](SSHORE.md#shell-ownership-and-attachment).

## Requests and approval

| Term | Meaning |
| --- | --- |
| Request | An immutable proposed copy, mount or forward held in daemon memory until local review or expiry. A pending approval is this request's state, not another object. Receiving or selecting a request grants no permission. |
| Approval | The user's explicit acceptance of the local endpoints in `review`, after source validation. Consent means the same thing; SSH authentication alone is insufficient. Approval is single-use. |
| Recovery | A transient shell on the authenticated SSH connection when normal sshore operation is unavailable. It has no sshore persistence or normal management features and is not a sandbox. |

An approved copy runs on a separate file stream without terminal attachment. An approved mount or forward becomes
an independent resource and retains the request's ID and NAME.
Ending a source shell does not stop its independent resources.

`origin:` and `:` name the current attached client's endpoint, not the computer
that originally created the shell. A path or port supplied by a remote request
is an editable local default, not permission to use it. Command syntax, endpoint
selection and standard-stream handling belong to `cp --help` and `forward --help`.

## Identity and state

| Term | Meaning |
| --- | --- |
| ID / NAME | A task has one immutable UID generated from 16 cryptographically random bytes and encoded as 32 lowercase hexadecimal characters; the first six characters are its displayed reference. An optional NAME is a readable label set at creation or by `rename`; it identifies the same task. References are scoped to the managing account. A prefix must identify one task; collisions require a longer prefix, the full UID or an explicit scope. |
| Account identity | A private identifier used to group account display records. It is not SSH authentication; copying its state can duplicate it. `reset` replaces it. |

### Task ID

The UID identifies the instance; its prefix is the short form of that same ID.
NAME is optional and does not change the ID or lifetime. Shells, mounts, forwards
and pending requests share the managing computer's name namespace.
Allocation and reference rules belong to `task_identity.go`.

Lists, cached connections, peer records and exit history are observations, not
authorization. A running worker does not prove its SSH connection or target is
healthy. Missing observations mean unknown state, not confirmed termination.
See [inventory and diagnostics](SSHORE.md#inventory-and-diagnostics).
