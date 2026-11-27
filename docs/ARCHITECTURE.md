# Architecture

sshore resolves client configuration and establishes SSH connections in Go.
`internal/sshconfig` evaluates rules without opening connections. It applies
command-line options before user and system files, retains earlier values on
the final pass, and preserves file lists as separate entries.
`internal/ssh` selects credentials and applies host-key and algorithm policy to
the Go SSH library. The library handles the wire protocol, key exchange, signing
and channels. The root package owns command policy, authorization and task
lifecycles. For exact behavior, start at the command entry point and follow it
to the platform implementation and its tests.

Each client command selects its SSH service. The remote entry point checks the
release and passes operation-specific arguments to the service handler.

- **How is a command parsed and dispatched?** [Command definitions, help and
  parser](../cli_schema.go), [dispatch](../cli.go),
  [launch confirmation](../internal/termui/launch.go)
- **How does a client connect and authenticate?** [Client connections](../client_connection.go),
  [configuration evaluation](../internal/sshconfig/), [SSH transport and host
  verification](../internal/ssh/)
- **How are installation and approval enforced?** [Installation](../install.go), [approval
  handling](../review.go), [request validation](../review_store.go)
- **How do copy, mount and forwarding reach their target?** [Resource commands](../cli.go), [shell
  routes](../resource_route.go), [filesystem operations](../internal/files/),
  [forwarding](../forward.go)
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

Local and hosted terminals share [process startup](../tmux_startup.go) and
[launch argument and directory rules](../internal/shell/).
`internal/shell` also resolves the default shell and parses command arguments.
PowerShell startup embeds the [directory reporting script](../internal/shell/pwsh_directory.ps1).
`new -- PROGRAM` passes an argument vector to the managed PTY.
`sh HOST -- COMMAND` uses SSH exec with standard streams and no PTY.
The [tmux layer](../internal/tmux/) owns terminals, input reservations and pin
expiry for `sh` and `new`. The daemon handles wire messages and exit reporting.
The account daemon registers local and hosted shells through the same task
lifecycle and reads their state from the managed terminal.
The [terminal tree](../internal/tmux/tree.go)
reserves foreground input while an attachment starts and rolls back the
reservation and any new parent edge if setup fails. The local and hosted
entry points handle their own handoff and resource commands.
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
`CGO_ENABLED=0 go run ./scripts go build -o sshore .`. It targets
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
For a cross-host child the initiating daemon holds an authenticated SSH route
scoped to the parent. Re-entering that child uses the same route. Each daemon
makes its own attachment decisions. Explicit detach records a pin before
releasing input and keeps the existing parent edge. When the owner ends, a
pinned child survives as a root with its own descendants. An internal handoff
preserves the original scope and pin. See [tree admission](../internal/tmux/tree.go),
[retention](../tmux_retention.go) and [attachment protocol](../tmux_attachment.go).

The control stream starts with an `attach` request carrying the client release and single-use
terminal token. The daemon checks the release before claiming the token and reports its release
in `ready`.

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

See [network topology](NETWORK.md) for SSH channels and [mount](mount.md) for worker startup,
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
Each accepted TCP connection gets a stream in the forward rule's smux session.
Intermediate hosts relay the resource stream as bytes.
The [forwarding package](../internal/forward/forward.go) owns listeners, target
dials and worker shutdown. The application supplies the authenticated transport
and the startup exchange for the approved rule.
The upgraded stream carries readiness and the caller's commit to the daemon.
The mount worker reports readiness once and keeps its control pipe to the daemon
after the launch connection closes.
The CLI formats SSH and mount diagnostics from callbacks. Copy clients and
mount clients use `pkg/sftp/v2` alongside the embedded agents.
The [files package](../internal/files/mount_paths.go) prepares local mountpoints.
The pinned upstream revision includes the client disconnect fix and supports the
project's Go version. Directory change notifications use their own stream.
Trace changes through the [task registry](../daemon_tasks.go), [mount coordinator](../mount.go)
and [route authorization](../resource_route_host.go). Consult [SECURITY.md](SECURITY.md)
before changing update, reset or approval behavior.

### Inventory and diagnostics

`pstree` follows shell parent UIDs across hosts. Both views group by SSH destination, with
`local` first. `list` shows the observed SSH address, OS, architecture, and hostname when
available. Conflicting snapshots for the same SSH name leave only the name.
Both views read live tasks, incoming leases, and `connections/participation.db`. Old participation
JSON is ignored. If the history fails, both views show live tasks and report the error.
Listing starts no daemon and sends no worker probes.
A shell whose attachment transition holds its lock appears as unknown. Starting and exited
shells are omitted when their state is known. The hosted shell list follows `setup_ready` on the same exec
stream. A live registration takes precedence over an observation of the same UID.

Program sessions appear as `run`. The managing account lists arguments alongside the executable.

On Windows, the command console and child shell shortcut use the last reported
filesystem location when available. Shells can report it through `OSC 9;9`, including
while detached. Inventory uses the reported or startup directory from the registry.

Task commands resolve names and UID prefixes through the
[task registry selector](../task_selection.go). A bare host in `attach` selects
a remote account when there is no matching task name. Attachment binds the full
UID before handoff.
Connection history supplies SSH settings. Incoming peer records are advisory.
See [inventory rendering](../inventory.go),
[daemon metadata](../daemon_metadata.go), and [integration tests](../tests/integration/).
