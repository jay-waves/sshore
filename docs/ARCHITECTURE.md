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

- **How is a command parsed and dispatched?** [Command definitions and help](../cli_help.go),
  [parser](../cli_schema.go), [dispatch](../cli.go)
- **How does a client connect and authenticate?** [Client connections](../client_connection.go),
  [configuration evaluation](../internal/sshconfig/), [SSH transport and host
  verification](../internal/ssh/)
- **How are installation and approval enforced?** [Setup](../setup.go), [approval
  handling](../review.go), [request validation](../review_store.go)
- **How do copy, mount and forwarding reach their target?** [Resource commands](../cli.go), [shell
  routes](../resource_route.go), [filesystem operations](../internal/files/),
  [forwarding](../forward.go)
- **Where is behavior tested?** [External integration tests](../tests/integration/) run the built
  CLI against OpenSSH, and installer checks run the platform scripts

## Terminal and mux design scope

Linux and Windows 11 / Server 2025 or newer are core amd64 client and host platforms.
Windows uses ConPTY for hosted shells.
Each interactive shell owns one terminal with one attachment at a time.
Split panes, tiling, screen emulation and scrollback replay are outside scope.
The shell and terminal preserve interactive state across detach. Redraw is best
effort and must preserve user input. See the [support matrix](../README.md#os-and-shell-support)
and [native PTY adapters](../internal/pty/).

Local and hosted shells share [process startup](../shell_startup.go),
[attachment and input release](../shell_attachment.go), and
[exit reporting](../shell_terminal.go). The [shell tree](../shell_tree_control.go)
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
manage mounts and forwards. Linux bootstrap tests cgroup creation, process launch
and group termination on the target account before resetting it. If the probe
fails, bootstrap reports the reason and continues the installation.

Build the experimental Apple Silicon client on Apple Silicon with
`CGO_ENABLED=0 go run ./scripts go build -o sshore .`. It targets
SSH connections to Linux hosts and supports local interactive shells. Install
the client manually. Local SFTP mounts use the traditional macFUSE kernel
extension through go-fuse. Install macFUSE and enable its kernel extension before
mounting.

TODO: Add a macFUSE FSKit backend that preserves `CGO_ENABLED=0`.

TODO: Support a Windows hosted shell as the intermediate hop in a nested shell route.

TODO: Run explicit `bash -c` in the command console on the host and in the directory
shown by the prompt.

Cross-compilation alone does not validate macOS terminal input, SSH
authentication, daemon lifetime or native mounts.

### Shell ownership and attachment

The account daemon owns the parent edges between its shells. It admits one
foreground terminal per tree. From outside, a frontend enters an idle root.
From a suspended parent it can enter a direct child, or adopt an idle root
and enter it in the same operation. A failed attach rolls back a provisional
adoption. A private reservation keeps the parent path occupied until the
frontend returns or disconnects. Input elsewhere in the same tree is refused.

A root follows its starting connection. A native local root uses the lifetime
of its loopback connection so system sleep does not expire its lease.
A same-host child follows its parent.
For a cross-host child the initiating daemon holds an authenticated SSH route
scoped to the parent. Re-entering that child uses the same route. Each daemon
makes its own attachment decisions. Explicit detach records a pin before
releasing input and keeps the existing parent edge. When the owner ends, a
pinned child survives as a root with its own descendants. An internal handoff
preserves the original scope and pin. See [tree admission](../shell_tree_control.go),
[retention](../shell_retention.go) and [attachments](../shell_attachment.go).

The authenticated exit frame carries the exit code and terminal output offset.
The frontend drains the reported tail within a time budget and reports the
exit even if the tail is incomplete. The local root prompt opens before loading
the shell tree. When the local account daemon shuts down, attached local clients
exit to the invoking terminal. Remote exits and detaches return to the parent
or the invoking terminal. Exit notices report surviving pinned children directly.
After a local approval or cancellation, the client resumes the requesting shell.
A connection loss without an exit frame leaves the state unconfirmed. Exit notices
identify the restored
shell, local console or calling terminal and whether pinned children remain.

### Account daemon and mounts

One authenticated daemon per OS account owns registered shells and resources.
The daemon, installer, and state storage share the [file lock adapter](../internal/sys/file_lock.go)
backed by `gofrs/flock`. Daemon startup is serialized by the account startup lock. The owner lock
identifies the live daemon, and its endpoint record supplies the address.
Confirmed shutdown closes task admission, stops the reviewed tasks in parallel
and waits for their results. A failed stop reports the task and reopens admission.
The shutdown deadline bounds the response while accepted cleanup completes.
Mounts and forwards use the same cleanup wait for individual and bulk stops.
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
The upgraded stream carries readiness and the caller's commit. The mount worker
uses a separate pipe exchange because it can outlive the launch connection.
The CLI formats SSH and mount diagnostics from callbacks. Copy clients and
mount clients use `pkg/sftp/v2` alongside the embedded agents.
The [files package](../internal/files/mount_paths.go) prepares local mountpoints
for both foreground mounts and background workers.
The pinned upstream revision includes the client disconnect fix and supports the
project's Go version. Directory change notifications use their own stream.
Trace changes through the [task registry](../daemon_tasks.go), [mount coordinator](../mount.go)
and [route authorization](../resource_route_linux.go). Consult [SECURITY.md](SECURITY.md)
before changing update, reset or approval behavior.

### Inventory and diagnostics

`pstree` groups tasks by managing host and follows shell parent UIDs across hosts.
`list` shows task details and incoming resource leases. Both read daemon snapshots,
including cached remote observations. A live registration takes precedence over an
observation of the same UID. Invalid records identify the affected source and field.

On Windows, the command console and child shell shortcut use the last reported
filesystem location when available. Shells can report it through `OSC 9;9`, including
while detached. Inventory also displays the location when available.

Task commands resolve names and UID prefixes through the
[task registry selector](../task_selection.go). An explicit scope selects the
managing account before matching. Attachment binds the full UID before handoff.
Incoming peer records are advisory. See [inventory rendering](../inventory.go),
[daemon metadata](../daemon_metadata.go), and [integration tests](../tests/integration/).
