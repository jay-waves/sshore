# Architecture

sshore resolves client configuration and establishes SSH connections in Go.
`internal/sshconfig` evaluates rules without opening connections. It applies
command-line options before user and system files, retains earlier values on
the final pass, and preserves file lists as separate entries.
`internal/ssh` selects credentials and applies host-key and algorithm policy to
the Go SSH library. The library handles the wire protocol, key exchange, signing
and channels. The command package owns command policy and authorization.
For exact behavior, start at the command entry point and follow it
to the platform implementation and its tests.

Each client command selects its SSH service. The remote entry point checks the
release and passes operation-specific arguments to the service handler.

- **How is a command parsed and dispatched?** [Command definitions, help and
  parser](../cmd/sshore/cli_schema.go), [dispatch](../cmd/sshore/cli.go),
  [launch confirmation](../internal/termui/launch.go)
- **How does a client connect and authenticate?** [Client connections](../cmd/sshore/client_connection.go),
  [configuration evaluation](../internal/sshconfig/), [SSH transport and host
  verification](../internal/ssh/)
- **How are installation and approval enforced?** [Installation](../cmd/sshore/install.go), [approval
  handling](../cmd/sshore/review.go), [request validation](../cmd/sshore/review_store.go)
- **How do copy, mount and forwarding reach their target?** [Resource commands](../cmd/sshore/cli.go), [shell
  routes](../cmd/sshore/resource_route.go), [filesystem operations](../internal/files/),
  [forwarding](../cmd/sshore/forward.go)
- **Where is behavior tested?** [External integration tests](../tests/integration/) run the built
  CLI against OpenSSH, and installer checks run the platform scripts

## Terminal and mux design scope

Linux and Windows 11 / Server 2025 or newer are core amd64 client and host platforms.
Windows uses ConPTY for hosted shells.
Each interactive task owns one terminal with one attachment at a time. `shell` and
`new` follow the starting connection or parent unless pinned or explicitly detached.
`new` reviews the launch in the client's full-screen console before attaching
the terminal. With `-- PROGRAM` it starts the executable directly. Otherwise it
opens a shell.
`Ctrl+B s` opens the [command console](approval.md#command-console).
Use `pstree` to display the task tree.
Enter a task ID in the console to attach. `Ctrl+B a` attaches the most recent
eligible session on the current host.
Attaching to the direct parent from the command console detaches the current
session and restores the parent's reserved terminal. A short ID is checked
against other matching tasks before the current session is detached.
Split panes, tiling, screen emulation and scrollback replay are outside scope.
The shell and terminal preserve interactive state across detach. Redraw is best
effort and must preserve user input. See the [support matrix](../README.md#os-and-shell-support)
and [native PTY adapters](../internal/pty/).

The [tmux manager](../internal/tmux/manager.go) owns live local and hosted sessions.
On first attachment, [tmux startup](../internal/tmux/startup.go) uses the
[shell package](../internal/shell/) to choose the executable and profile options,
then starts and owns the PTY. `new -- PROGRAM` supplies an explicit argument vector.
PowerShell startup embeds its [directory reporting script](../internal/shell/pwsh_directory.ps1).
`sh HOST -- COMMAND` uses SSH exec with standard streams and no PTY.
The account daemon supplies the working directory and environment and publishes task rows to bbolt.
The initiating client owns SSH. The host daemon authenticates each incoming task stream.
The daemon resolves selections by full task ID. The [terminal tree](../internal/tmux/tree.go) reserves input during attachment
and rolls back the reservation and any new parent edge if setup fails.
The [PTY package](../internal/pty/) owns child processes, native handles, sizing
and process cleanup. The [terminal package](../internal/term/) handles keyboard
shortcuts, control sequences and the local console. Both use platform files
for native system calls.

The daemon observes terminal modes while a shell runs, including detached
output. The frontend observes the stream it displays. A fresh attachment and
the return from the command console both establish the same input baseline,
apply the application mode snapshot, then resume any partial control sequence.
Only the Windows shell host collects directory reports. Output observation,
Windows filtering and keyboard control strings share the same sequence-boundary
rules.

Linux shells require cgroup v2 with `cgroup.kill` and `CLONE_INTO_CGROUP`. The
daemon uses its own writable delegated subtree or requests a delegated scope
from the systemd user manager. Each shell starts directly in its own child
cgroup. Ending the shell task terminates remaining members, including background
jobs that created separate sessions. Without delegation the daemon can still
manage mounts and forwards. A shell launch reports any delegation error.

Build the experimental Apple Silicon client on Apple Silicon with
`CGO_ENABLED=0 go run ./scripts go build -o sshore ./cmd/sshore`. It targets
SSH connections to Linux hosts and supports local interactive shells. Install
the client manually. Local SFTP mounts use the traditional macFUSE kernel
extension through go-fuse. Install macFUSE and enable its kernel extension before
mounting.

TODO: Add a macFUSE FSKit backend that preserves `CGO_ENABLED=0`.

TODO: Run explicit `bash -c` in the command console on the host and in the directory
shown by the prompt.

Cross-compilation alone does not validate macOS terminal input, SSH
authentication, daemon lifetime or native mounts.

### tmux ownership and attachment

The tmux runtime owns the parent edges between managed shells. It admits one
foreground terminal per tree. From outside, a frontend enters an idle root.
From a suspended parent it can enter a direct child, or adopt an idle root
and enter it in the same operation. A failed attach rolls back a provisional
adoption. A private reservation keeps the parent path occupied until the
frontend returns or disconnects. Input elsewhere in the same tree is refused.
Cycling between idle roots keeps each root in its existing tree.

A root follows its starting connection. A native local root uses the lifetime
of its loopback connection so system sleep does not expire its lease.
A same-host child follows its parent.
Running `sshore sh B` inside A starts a foreground SSH client in A's terminal.
B hosts an independent root session. In B, `sshore detach` returns to A and retains B.
The outer client handles its shortcut prefix. Press `Ctrl+B` twice to pass the prefix to B.
`exit` ends B and returns to A. Explicit detach records a pin before
releasing input and keeps the existing parent edge. When the owner ends, a
pinned child survives as a root with its own descendants. An internal handoff
preserves the original scope and pin. See [tree admission](../internal/tmux/tree.go),
[retention](../internal/tmux/retention.go) and [attachment protocol](../cmd/sshore/shell_attachment.go).

The daemon checks the client release and pairs terminal channels with a
[single-use token](../cmd/sshore/terminal_endpoint.go). tmux takes the connected
stream and holds its input reservation while sending `ready` and replaying modes
with the session lock released. Stopping the session cancels a pending attachment.
The daemon closes unclaimed streams on exit and checks approvals atomically.

The authenticated exit frame carries the exit code and terminal output offset.
The frontend drains the reported tail within a time budget and reports the
exit even if the tail is incomplete. A remote root shell returns its exit code to the caller.
The local root prompt opens before loading the shell tree. Local daemon shutdown returns
attached clients to the invoking terminal. A nested remote exit returns to the parent.
Exit notices report surviving pinned children directly.
After a local approval or cancellation, the client resumes the requesting shell.
The task completes after its attachment and PTY close. A connection loss without an
exit frame leaves the state unconfirmed. The exit notice identifies the restored shell,
local console or calling terminal and whether pinned children remain.

### Account daemon and mounts

See [network topology](network.md) for SSH channels and [mount](mount.md) for worker startup,
directory caching and notifications.

The local client uses one authenticated daemon per OS account. Remote deployments
use one daemon per client ID and release. Each daemon owns its registered tasks.
The daemon, installer, and state storage share the [file lock adapter](../internal/sys/file_lock.go)
backed by `gofrs/flock`. Daemon startup is serialized by the account startup lock. The owner lock
identifies the live daemon, and its endpoint record supplies the address.
Confirmed shutdown stops the reviewed task set. A stop signal to the daemon process
stops its current task set. Both close admission and wait for parallel cleanup.
A failed stop reports the task and reopens admission. Accepted cleanup may continue
past the shutdown deadline. Individual resource stops use the same cleanup wait.
Normal connections require the exact full release version. Validate
[process identity](../internal/sys/process_identity_linux.go) before signaling.
A remote shell's mount
or forward approval is bound to the original task and its requested operation.
The daemon opens fresh SSH
connections through the authenticated route at startup. The resource owns those
connections after startup, so exiting a source or ancestor shell leaves the
resource running. Mount and forward configuration travels in the authenticated
launch request.
Each TCP connection or UDP sender gets a stream in the forward rule's smux session.
The [ports package](../internal/ports) owns endpoint discovery, listeners, target
dials and worker shutdown. The application supplies the authenticated transport
and the startup exchange for the approved rule.
The daemon task registry publishes mount and forward state at registration, readiness and exit.
The caller commits startup on the launch stream. After it closes, the daemon can still stop
a forward by cancelling its context or unmount through the mount worker's control pipe.
Copy and mount use `pkg/sftp/v2` with embedded agents. Directory notifications use their own stream.
The [files package](../internal/files/mount_paths.go) prepares local mountpoints.
Windows drive-letter mounts use unique WinFsp UNC paths for handle-based path resolution.
Automatic mounts retry if another process claims the selected drive.
TODO: Support handle-based canonical paths for Windows directory mounts. Windows
junctions require disk volumes, and WinFsp network volumes require drive letters here.
See the [task registry](../cmd/sshore/daemon_tasks.go), [mount coordinator](../cmd/sshore/mount.go),
[route authorization](../cmd/sshore/resource_route_host.go) and [security safeguards](SECURITY.md).

### Inventory and diagnostics

`pstree` follows shell parent UIDs within each daemon. Both views group by SSH destination, with
`local` first. `list` shows the observed SSH address, OS, architecture, and hostname when
available. Conflicting snapshots for the same SSH name leave only the name.
Clients record recent connections, detaches, and remote observations in `connections/participation.db`.
Remote output refreshes the attachment at most once a minute. Detach history ranks remote shell UIDs.
Both views and task selection read bbolt. Before serving, the account daemon replaces the live
rows in one transaction. Each daemon records its own sessions. A remote reattachment uses the saved SSH
connection for the selected UID.
Live rows are keyed by full UID. Short IDs are checked against saved observations
for ambiguity. A terminal and its affected ancestors commit together.
Readers omit starting and exited shells. The host reads its registered hosted shells from bbolt and
sends them after `setup_ready`. A live registration takes precedence over an observation of the same UID.

Program sessions appear as `run`. The managing account lists arguments alongside the executable.

On Windows, the command console and child shell shortcut use the last reported
filesystem location when available. Shells can report it through `OSC 9;9`, including
while detached. Inventory uses the reported or startup directory from the registry.

Task commands resolve names and UID prefixes through the
[task registry selector](../cmd/sshore/task_selection.go). A bare host in `attach` selects
a remote account when there is no matching task name. Attachment binds the full
UID before handoff. Direct remote tasks store their SSH routes. Host defaults use connection history.
Peer records under `origin` are read-only. See rendering for [list](../cmd/sshore/list.go) and [pstree](../cmd/sshore/pstree.go),
[shell connection history](../cmd/sshore/shell_history.go), and [integration tests](../tests/integration/).
