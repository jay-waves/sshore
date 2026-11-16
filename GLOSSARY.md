# Terms and relationships

This document defines the project vocabulary. Commands are documented by
`sshore COMMAND --help`; implementation and security rules belong in
[SSHORE.md](SSHORE.md) and [SECURITY.md](SECURITY.md).

## Machines and connections

| Term | Meaning |
| --- | --- |
| Local computer | The machine running the client or managing a mount/forward. Local is relative to that operation. |
| Remote host / SSH destination | The host reached through system OpenSSH, often named by a `~/.ssh/config` alias. |
| Remote account | The authenticated Unix account. Shell management excludes other users. |
| Client | The foreground sshore process attaching to a local or remote shell. It is not the shell itself. |
| Transport | The SSH connection carrying a shell attachment, independent forward or SFTP mount. |
| Attachment | One client's live connection to a shell. At most one client is attached at a time. |
| Peer / manager | Another computer managing a resource whose advisory record is visible on the remote account. |

## Shells and lifecycle

| Term | Meaning |
| --- | --- |
| Mux | The detach/reattach capability: explicitly leave a shell running and attach again later. Each shell session has one terminal; mux does not imply split panes. `internal/tmux` implements native terminals, ownership primitives, shell startup, attachment I/O and detach confirmation; it does not use the external tmux program. The internal smux library separately multiplexes transport streams. |
| Session | A running task managed by sshore: a shell, forward or mount. Pending reviews are requests, not started sessions. |
| Shell | A shell process and native terminal owned by a daemon, local on Linux/Windows or remote on Linux. `launch` creates a persistent session that survives detach and connection loss. |
| Spawn | A temporary shell created by `spawn`, displayed as `spawn/NAME`. It can detach and reattach but ends with its original owner. A remote spawn also ends on loss of its lifetime SSH connection. |
| Spawn owner | The parent shell process that launched `spawn`, identified by PID and creation identity. Attach never transfers ownership. |
| Lifetime SSH connection | A local-daemon-owned connection keeping a remote spawn alive independently of foreground attachments. It is not automatically reconnected. |
| Daemon | A background owner of native terminals and lifecycle. One local daemon per user/state root hosts multiple shells and forward transports, and supervises mount workers; Linux remote shells share a daemon per runtime directory, with separate helper endpoints. |
| Shell name | A public label, automatic or user-supplied; renaming it does not replace the daemon or shell. |
| Task | A context-dependent display term: a numbered item in `list`, or a program running under a shell. It is not a separate execution model; the managed lifecycle uses session, shell and resource. Identifying shell programs for termination requires process identity checks, not just a displayed PID. |
| Shell lifetime | Startup policy: `parent` for local spawn, `heartbeat` for remote spawn (original parent plus lifetime SSH), `persistent` for launch. Detach and attach never change this policy. These labels do not measure connection health. |
| Owner context | Display-only identity of the managed shell from which a spawn was started, when known. It stays fixed across attachments; it does not replace the actual spawn owner PID/creation identity, which may belong to a wrapper. |
| Attachment source | The managed shell from which the current attachment was opened, when known. It may change on reattachment and conveys no lifetime ownership. |
| Detach | Explicitly end an attachment while preserving its shell according to its startup lifetime. Ctrl+B d and `sshore detach` request this. |
| Disconnect | Loss of transport. It releases an attachment. Persistent shells survive; spawns survive only while their owner and, remotely, their lifetime SSH connection remain alive. |
| Cancel | The public action for discarding a request, stopping a resource or ending a named remote shell. Its scope depends on the identifier. |
| Recovery | A temporary shell on the authenticated SSH connection, without persistent shell or normal sshore operations. It is not a sandbox. |
| Release version | Full MAJOR.MINOR.PATCH identifier used for compatibility. There is no independent sshore protocol version counter. |

## Requests, resources and views

| Term | Meaning |
| --- | --- |
| Request | Bounded metadata proposing a file operation or forward; receiving it grants no permission. |
| Pending approval | A local single-use request record, identified as `review/NAME`, awaiting user review. The public name is reserved for its action; an approved forward retains it. Its random internal request ID remains distinct. |
| Review | The separate local command that selects a pending request, collects consent and attempts execution. |
| Consent / approval | The user's explicit acceptance in review. SSH authentication alone is not consent. |
| Copy | A regular-file operation expressed as `cp SOURCE... TARGET`; not full system cp or scp compatibility. |
| Tunnel | The README feature category for TCP port forwarding through SSH. |
| Transfer | A one-time regular-file copy. Both direct and approved copies use SFTP; approved copies run on a client-initiated shell stream after local review. |
| Resource | An independently managed local mount or forward, with its own transport and lifetime. Its ID is `TYPE/NAME`, such as `forward/pikachu` or `mount/eevee`; the type prefix identifies the object kind. Shells, resources and pending reviews share a local name namespace and default Pokemon pool. |
| Job | The Windows Job Object used to contain a local shell process tree; not an application-level task type. |
| Worker | A separate local process owning a mount and its SSH transport, started and reaped by the local daemon. Forwards run directly under the daemon; older independent forward workers remain manageable. |
| Mount | A remote directory exposed to local applications through SFTP and FUSE/WinFsp, identified as `mount/NAME`. |
| Recent connection | A private local default for a previously connected host/shell and its SSH transport settings; it is not proof that the remote shell is still available. |
| Mount agent | A transient remote sshore process that carries SFTP and filesystem notifications over a mount-owned SSH connection; it is not a persistent shell daemon. |
| Forward | A fixed TCP port mapping through SSH, identified as `forward/NAME`. It can carry an existing SOCKS service; it does not itself implement SOCKS. |
| Native forwarding | OpenSSH `-L/-R/-D` configured for a connection, separate from a managed `forward` resource. `-D` creates a SOCKS proxy. |
| Inventory | A diagnostic view of resources, requests, shells or peer records. It is not an authorization source. |
| Peer record | A remote advisory mirror, identified as `peer/KEY/RESOURCE`; it cannot control the original resource. |
| Receipt | A bounded diagnostic record of a shell/resource ending. It is not proof of complete task cleanup or remote write outcome. |

## Relationships and scope

- A **client** attaches through a **transport** to a **shell** owned by a remote
  **daemon**. Detach removes the attachment, not the shell.
- A remote helper creates a **request**. Local **review** obtains consent and checks
  its source shell before attempting a transfer or starting a forward.
- A **transfer** belongs to its executing connection. An approved **forward** becomes
  an independent resource and leaves the source shell detached.
- A **mount** is created explicitly on its managing computer; it is not a remote
  approval request. Ending a shell does not stop its independent resources.
- `cancel review/NAME` discards a request; `cancel mount/NAME` or
  `cancel forward/NAME` stops that local resource; `cancel HOST NAME` ends that
  remote shell and its identified tasks. A peer record is read-only.
- `origin:` and `:` name the **client endpoint** of the current connection,
  even inside a remote shell. This is the current attached client, not the
  computer that originally created the persistent shell.
- A bare `-` in direct `cp` names the invoking process's **standard stream**:
  stdin as source, stdout as target. It does not name the client computer or
  request an interactive selection. `origin:-` and `./-` name an ordinary file.
- An empty client endpoint is selected locally. A filled `origin:PATH` or
  `origin:PORT` directly specifies a local operation's endpoint; in a remote
  request it is a **local hint**, displayed as an editable review default.
- In `forward LISTENER SERVICE`, the **listener** accepts connections and the
  **service** receives them. The **pending local port** must be selected or
  accepted locally before the mapping becomes an executable forward.
- Submitting all valid local paths or the local port constitutes **consent**.
  Selecting a request or completion candidate alone does not.
- **Worker running**, **transport connected** and **target healthy** are different
  claims. Listings do not probe target services; missing peer heartbeats mean
  unknown state, not confirmed termination.

Business filename prefixes follow the public commands: `spawn`, `cp`,
`mount`, `forward` and `review`. `internal/files` is the shared, flat implementation package for `cp` and `mount`;
its shared SFTP and file primitives belong to neither command adapter.
The mount protocol is SFTP over SSH. `review` replaces
`approve`, and `cancel` replaces the public `stop`/`kill` verbs. Internal wire or
function names may retain historical terminology.

## Task ID

A persistent numeric shortcut assigned by the local manager to a task instance.
It is stable across rename, detach and reconnect, increases without reuse, and
is distinct from a shell PID, public name, approval ID or temporary picker index.

An **account identity** is a locally salted OS identifier used only to group host display records. It is not an authentication identity; cloning its state can duplicate it. `sshore reset` replaces it.
