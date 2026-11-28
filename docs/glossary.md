# Terms and relationships

## Tasks

- **Task** A daemon-owned item: a terminal shell, mount or forward. One in-memory account table owns
  its identity and lifecycle; each kind retains its execution mechanism. Programs running inside a
  shell are its processes, not another sshore task type.
- **Shell / session** A daemon-owned shell process with one terminal. Local shells run on Linux or
  Windows; remote shells currently run on Linux. A session means a shell session, never a mount or
  forward.
- **Pin** Retention on a shell. `sh --pin` retains it until exit. `detach [DURATION]` retains the
  running shell as it releases its terminal.
- **Mount** A remote directory exposed to local applications through SFTP and FUSE/WinFsp.
- **Forward** A fixed TCP or UDP port mapping through SSH. TCP forwards can carry existing
  SOCKS services. Use this term instead of tunnel.
- **Copy / transfer** A one-time regular-file copy through SFTP, invoked directly or executed after
  approval. It belongs to its executing connection.
- **Resource** A collective name for mounts and forwards, which have lifetimes independent of shell
  attachments. It does not introduce another task type.

## Connections and ownership

- **Local / remote** Relative to the operation: local is the computer running the client or managing
  the resource; remote is its SSH destination. The remote account is the authenticated Unix account,
  excluding other users.
- **Client** The sshore process interacting with the user. Each SSH login has its own client.
- **Origin** The client side of a connection. In an attached shell, `origin:` (alias `0:`)
  addresses the attached client. For a mount or forward, the origin account owns the task.
  On the SSH host, `list` and `ps` (`pstree`) group these read-only records under `origin`.
  Each record refers to its own connection, so several clients can appear in this group.
- **Daemon** The background process for an OS account, reached through one authenticated
  loopback TCP endpoint. It owns the terminals, activity leases and mount workers. On startup it
  replaces the live task list in bbolt and keeps the account's connection history.
- **Activity lease** A daemon-owned registration retained by a live authenticated connection or an
  in-process owner. The shared daemon heartbeat detects lost clients. Releasing a lease ends that
  participation; persistent shell lifetime remains independent.
- **Attachment** A client's live interactive connection to a shell. Each shell tree grants its
  keyboard to one foreground shell. Its suspended parents are shown as `waiting`. Explicit detach
  retains the shell before releasing the attachment. Loss of the terminal connection follows the
  current shell scope. Mux means this detach/reattach capability.
- **Transport** The connection carrying operations, usually SSH; local attachments use local control
  connections. The authenticated starting lease is separate from the foreground attachment.
- **Owner** The starting client lease or parent shell that supplies a temporary shell's lifetime.
  Entering an idle root from a shell on the same daemon makes that shell its parent. The account
  daemon applies pins and cleans up ended tasks.
- **Attachment source** The managed shell from which the current attachment was opened. It may
  change on reattachment and does not control the target shell's lifetime.
- **Peer record** The read-only description of an origin-owned mount or forward on its SSH host.

Same-host switches share the current client. An SSH login started in a shell uses that host's
SSH configuration and signing credentials. See
[connection ownership](ARCHITECTURE.md#tmux-ownership-and-attachment).

## Requests and approval

- **Request** An immutable proposed copy, mount or forward presented to the local user. Receiving a
  request grants no permission.
- **Approval** The user's explicit acceptance of a request and local endpoints, after source
  validation. Consent means the same thing; SSH authentication alone is insufficient. Approval is
  single-use.
- **Legacy recovery** A temporary shell offered by older releases when setup could not complete.
  Current setup aborts instead; inherited disabled-mode restrictions remain until reconnecting.

An approved copy runs on a separate file stream. An approved mount or forward
becomes an independent task with its own UID and name.
Ending a source shell does not stop its independent resources.

`here:` and `.:` identify the machine running the command. A remote request's path or port is an
editable default in local approval.

## Identity and state

- **ID / NAME** A task has one immutable UID generated from 16 cryptographically random bytes and
  encoded as 32 lowercase hexadecimal characters; the first six characters are its displayed
  reference. An optional NAME is a readable label set at creation; it identifies the
  same task. References are scoped to the managing account. An ID selector accepts 2–32 hexadecimal
  characters and must identify one task; collisions require a longer prefix, the full UID or an
  explicit scope.
- **Account identity** A private identifier used to group account display records. It is not SSH
  authentication; copying its state can duplicate it. `kill --all --reset` replaces it.

### Task ID

The UID identifies the instance; its prefix is the short form of that same ID.
Shells, mounts and forwards share the managing computer's name namespace.
The daemon claims the name when it registers the task. It rejects a requested name
already in use. For an unnamed task it uses the shortest available prefix of the UID
starting at six characters.

The selected host resolves task IDs in its live registry. Saved connections
supply SSH settings and routes for reconnecting. Missing observations leave task state unknown.
See [inventory and diagnostics](ARCHITECTURE.md#inventory-and-diagnostics).
