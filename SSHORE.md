# sshore development

Read [AGENTS.md](AGENTS.md) for contributor rules, [GLOSSARY.md](GLOSSARY.md)
for terminology and [SECURITY.md](SECURITY.md) for security guarantees.
[README.md](README.md) is the user manual; CLI help owns syntax and defaults.

Code and tests own implementation details; this guide covers ownership, design
constraints and development workflows.

## Architecture and code map

sshore is a Go CLI using system OpenSSH only to evaluate configuration (`ssh -G`).
A system SSH agent or interactive credentials authenticate; Go owns the connections.
The root program coordinates commands, release policy and cross-component
lifecycles. Internal packages provide mechanisms without owning application
policy or depending on mutable application state.

| Responsibility | Start here |
| --- | --- |
| One-off SSH execution | `cli.go`: exec adapter uses the shared SSH connection layer without remote setup |
| CLI grammar, dispatch and help | `cli_help.go`: command definitions, help and examples; `cli_schema.go`: aliases and product/SSH parsing; `cli_commands.go`: command semantics; `cli.go`: dispatch; `cli_tasks.go`: short-ID selection |
| Mount command and execution | `cli_resources.go` handles resource command entry; `mount.go` coordinates the operation and adapts SSH and routed connections |
| Connections and session switching | `client.go`, `client_connection.go`; saved connection records in `daemon_metadata.go` |
| Prepared installation and exact-release policy | `setup.go` handles version checks and installation policy. `setup_linux.go` handles remote service startup and Linux installation. `setup_windows.go` handles Windows installation |
| Shell hosting, identity and cancellation | `launch_*_linux.go`, `shell_lifecycle_linux.go`, `server_linux.go`; process inspection in `process_linux.go` |
| Account daemon and local shell control | `daemon*.go` (task registry and resource control in `daemon_tasks.go`, plus activity leases and metadata), `local_command.go`, `local_shell.go`; hosted Unix shell adapters in `platform_*.go` |
| Spawn ownership, lifetime SSH and nested routes | `spawn_transport.go` coordinates owner leases and relay lifetime; `spawn_route_linux.go` and `platform_*.go` handle nested routes |
| Shell startup and attachment I/O | `shell_terminal.go` shares terminal startup, output and input lease handoff between native and SSH-hosted shells. `internal/pty/` owns native terminal hosts |
| Native PTY and ConPTY hosts | `internal/pty/` |
| Shell attachment protocol and endpoints | `protocol.go`, `shell_attachment.go` |
| Console I/O and shortcuts | `internal/pty/` |
| CLI prompts and line editing | `internal/termui/` |
| Process identity, verified signaling, display names, Linux process snapshots and child waiting | `internal/proc/`; `pstree.go` decides when to display an external owner; application code retains task scope and termination policy |
| Local prompts and presentation | `internal/termui/`, application adapters in `cli_ui.go` |
| SSH connections | `internal/ssh/` owns SSH authentication, host verification, jump connections and channel primitives |
| Multiplexed relays | `spawn_transport.go` owns the local frontend-to-daemon mux and spawn ownership. `internal/forward/` owns forwarding relays and multiplexing |
| Copy and mount mechanisms | Flat `internal/files/`; command entry in `cli_resources.go`, operations in `cp_command.go` and `mount.go` |
| Pending requests and atomic approval | `review_store.go`, application coordination in `review.go` (including shared source-shell validation in `review_linux.go`) |
| Forward transport and retries | `forward.go`: endpoint parsing, command execution and direct SSH forwarding lifecycle |
| Shell-routed resources | `resource_route.go` opens the route; `resource_route_linux.go` checks release and approval before serving file, mount and forward endpoints |
| Resource startup and control | `resource_runtime.go`, `resources.go` |
| Private storage and name reservations | `state.go` (including account identity and remembered entry points), `task_identity.go`; native checks in `platform_*.go` |
| Inventory, stable IDs and shell relationships | `inventory*.go`, `list*.go`, `shell_cache.go`, `pstree*.go`, `peers*.go` |
| Platform adapters | `platform_linux.go` / `platform_windows.go` in the application and file/transport packages; `console_linux.go` / `console_windows.go` in terminal/UI packages |
| Product workflow tests | `tests/`: shared isolated CLI build, public command workflows and native terminal handoff; module and private protocol regressions remain with their owning code |
| Installation, packaging and publication | `install.sh`, `install.ps1`, `scripts/`, `.github/workflows/` |

Keep command policy in the application: authorization, release checks, prompts,
identity validation and resource coordination. Pass validated configuration and
small interfaces to mechanisms. Extend the owning module before adding an
abstraction; move behavior with its tests and platform implementations.

## Documentation ownership

Keep project documentation in the five root files. CLI help owns syntax and defaults;
code and tests own implementation details. Link to the authoritative source rather
than repeating it.

| Document | Responsibility |
| --- | --- |
| [README.md](README.md) | Introduction, common workflows and essential consequences |
| [SSHORE.md](SSHORE.md) | Architecture, development, validation and releases |
| [SECURITY.md](SECURITY.md) | Trust boundaries, safeguards and remaining risks |
| [GLOSSARY.md](GLOSSARY.md) | Terms and identities |
| [AGENTS.md](AGENTS.md) | Essential contributor constraints |

Keep modules cohesive with clear ownership and small interfaces; platform and test
build constraints may justify separate files. Use `tests/` for public workflow tests
and keep module tests beside their owners. Group tests by behavior and preserve
platform isolation. Keep the ownership map and affected README/SECURITY Mermaid
diagrams current. Check links, anchors and fences after documentation changes,
including packaged documents. Never modify published assets to update documentation.

## Design constraints

### CLI parsing and help

`cli_schema.go` parses command definitions; command adapters interpret parsed operands.
Shell creation and attachment normalize their endpoints before the adapter
builds connection options.
`cli_help.go` keeps each command's operand limits and options alongside its
help and concise workflow examples (`--tldr`).
Keep options, aliases, arity and shared conflicts in the schema. Parsing and help
have no execution side effects. `kill` is the termination command; do not add a
`cancel` alias or a separate protocol compatibility counter.

### Terminal and mux design scope

Support Linux and Windows **amd64 only**, with remote shell hosting on Linux.
See the [OS and shell support matrix](README.md#os-and-shell-support) for
supported systems and shells. Windows provides local ConPTY sessions and SSH
clients, not a remote shell server.
ConPTY children join their kill-on-close Job during process creation. ConPTY
uses synchronous pipe handles. The daemon uses overlapped handles for input
writes and output reads. Input deadlines let Linux PTYs and Windows ConPTY
share the same input lease. The Windows frontend pins its native keyboard reader
and uses CancelSynchronousIo to release it before transferring stdin ownership.
ConPTY startup releases child-facing pipe ends before rollback or starting the
Windows thread-pool process wait. The Windows environment keeps the last value
for each case-insensitive name and emits one sorted block.
Natural exit and explicit close share the same Job and pseudoconsole shutdown.
The client receives the shell's exit code after the terminal output is drained.
On Linux, the Go I/O poller waits for shell exit through a pidfd when available.
The host then reaps the shell and bounds the remaining PTY output drain.
Each interactive shell owns one terminal and permits one attachment at a time.
Split panes, tiling, screen emulation and scrollback replay are outside scope.

The shell and native terminal retain interactive state. Redraw is best effort;
do not inject Enter, Ctrl+L or editor commands to reconstruct it. Terminal bytes
must retain order and bounded buffering. Unsupported terminal protocols must not
silently become local shortcuts.
On Linux, redraw briefly changes the PTY width and restores it so foreground
applications that respond to actual size changes can refresh their display.
The frontend leaves an alternate screen selected by the shell before returning
to the calling terminal. The host reapplies the shell's buffer selection on
reattachment before requesting a redraw.

### Shell environment for scripts

- `SSHORE_TTY` contains the Linux PTY device path for a hosted interactive
  shell, local or remote. The path stays the same across detach and reattach.
  Windows ConPTY has no Linux device path. Use `test -t 0` to check whether
  the current command's standard input is a terminal.
- `SSHORE_SOCKET` identifies a hosted remote shell. `SSHORE_LOCAL_SESSION`
  identifies a hosted local shell, including on Windows.
- `SSHORE_TREE_ID` identifies the managed shell in the process tree.

### SSH connections

`internal/ssh` owns one connection and any jump chain per task; closing an
attachment does not close a daemon-owned spawn lease. OpenSSH configuration is
resolved in the initiating environment and directory, including its remote
command. No persistent SSH subprocess,
ControlMaster, askpass executable or implicit reconnect is used.
The CLI, saved connections and ProxyJump use the same SSH option parser.
Shell and file startup, forward watchers and peer cleanup share the remote
executable lookup and version probe. Each command handles a version mismatch
according to its own protocol.
The CLI detects an SSH port while parsing the suffix. An explicit `--port`
used by itself replaces the saved port and keeps the other saved SSH options
in their original order.
Forwarding filters operate on whole options, including the attached forms of
`-L`, `-R`, `-D` and `-o`.

The server may accept SSH `none` authentication (for example Tailscale SSH).
Only when public-key authentication is requested is the system agent queried. Linux
uses a Unix agent socket; Windows uses its OpenSSH agent pipe. Configuration
resolution and agent connections are per connection. Unsupported execution hooks and authentication
modes fail explicitly. Keep resolver arguments compatible with Windows in-box
OpenSSH; Windows system path tokens use the resolver's initiating environment.
Each ProxyJump hop inherits explicit `-F` options and resolves its own
settings.
Public-key authentication tries certificates matching configured identities,
then their raw keys, then other agent keys when allowed. A certificate can use
the agent signer for its underlying key.
Modern algorithm negotiation uses the supported Go SSH
algorithms intersected with resolved configuration, preserving configured order
within the allowed set. SSH debug logging (`-v` or `LogLevel DEBUG`) reports the
negotiated key exchange, host-key algorithm and both traffic directions, marking
AEAD integrity protection. Normal connections remain quiet. Host verification uses the
local known_hosts policy; unknown-host confirmation is routed to the initiating
frontend over private local control. Background retries never prompt.
An interactive authentication prompt pauses the connection timeouts across the
active ProxyJump chain. Each hop resumes with its remaining time. One timer
tracks the earliest deadline, and caller cancellation stays active throughout
setup.
Each handshake watches either its direct TCP connection or its jump, which
owns the channel carrying the handshake. Authentication configures host-key
checks and login prompts together for each handshake. A separate hook covers
agent key listing and signing. The handshake retires its hooks and closes its
agent connection before handing off the authenticated client. Each client keeps its
immediate jump open for its lifetime and closes the chain starting from the
underlying TCP connection.

For remote launch, attach and spawn, the initiating daemon owns SSH and forwards
shell selection and authentication prompts to the frontend. After version checks it opens an SSH
TCP channel to the
account daemon's loopback endpoint. Native and SSH clients authenticate there and
open streams by immutable task ID. All three modes require the preparation
process to hand back an authenticated daemon endpoint before it exits. A remote spawn claims an
independent owner channel before reporting readiness; each data endpoint uses a
sibling native SSH channel. The owner reuses the daemon's pulse/alive lease
protocol and timeout, including a close timer for SSH channels without read
deadlines. SSH TCP forwarding must be permitted. A version mismatch stops the
connection before opening a shell.
Only an attach without a selected shell can accept a setup menu, and it can
answer one menu per handshake. File helpers read the remote home directory
from the setup reply when resolving paths.

Native port forwards belong to their connection. Application forwards use
separate connections. The initial attempt must complete startup before the
configured retries can begin. Retries open a fresh transport without repeating
the initiator handoff. Cancellation closes owned
connections to interrupt channel I/O before joining workers. Preserve this order
when adapting streams; session stdin must serialize EOF with writes.
Application forward listeners and local service dials use IPv4 loopback.
SSH AddressFamily selects the SSH endpoint's network.
SSH command runners drain stdout before waiting on normal exit. If reading or
forwarding stdout fails, they close the owning connection before joining the
command.
When a spawn's owner lease ends, its relay rejects new endpoints and gives
already-open connections up to two seconds to drain. Frontend detach leaves
the owner lease active.

### Shell ownership and attachment

Keep lifetime separate from interaction:

- `spawn` belongs to its original parent process; remote spawn also requires its
  lifetime SSH connection. Detach leaves that connection active. If that
  connection closes, its exit receipt records `spawn lifetime connection closed`,
  separately from an explicit kill or an already-recorded shell exit.
- `launch` is independent of the launching client. Detach, switching and
  reattachment never change a shell's startup lifetime or transfer its owner.
- Attachment is exclusive. A stale attachment must not release its successor.
  Failed creation may roll back its new terminal; failed reattachment must
  preserve the existing shell.
- Cancel and join old input readers/writers before handing the terminal to
  another attachment, local prompt or SSH authentication process.

Ctrl+B c always requests a nested spawn on the current shell's host, through
the existing daemon handoff and return stack. Its owner is the current managed
shell, with its recorded process identity, startup directory and environment.
It never inherits launch lifetime or injects a command into terminal input.
Exit or detach returns one level; the parent program continues running.

The client at the user's terminal owns terminal input and shortcuts.
Ctrl+B : or Ctrl+B s opens a frontend command prompt using the terminal screen.
`internal/termui` draws the prompt and runs Readline. The frontend owns terminal
handoff, command dispatch, and the completion data. `internal/pty` handles the
screen transition and input-mode restoration. The frontend uses the same validated
session targets and instance-based switching as other shortcuts. Tab completes
the common prefix or cycles matching candidates in the input line, keeping the
full-screen layout stable for long paths and large directories. Readline's erase
and redraw are sent together with synchronized-output markers to reduce flicker;
the markers are ignored by terminals that do not support them.
The prompt’s launch opens a persistent shell on the current host. Detach or
exit returns to the previous attachment. On a hosted shell the return route
may end with the parent, while the new launch remains independently attachable
by ID on its host.
While the prompt has focus, PTY output is buffered up to 256 KiB. If the buffer
fills, the prompt closes and output resumes. On return, the frontend requests a
redraw for the underlying application.

Ctrl+B n and Ctrl+B p select the next or previous validated target by name
and wrap at the ends. The client keeps the selected target's instance ID.
`x/ansi` provides sequence framing and parameter parsing; the shortcut adapter
recognizes legacy controls, CSI-u/Kitty and xterm modifyOtherKeys. Consumed keys'
repeat/release reports stay local; unconsumed input retains its original bytes.
Escape and incomplete CSI reports wait up to 50 ms of input inactivity before
being forwarded. Late CSI tails remain opaque, and retained sequences are bounded.
Opaque control strings, X10 mouse payloads and bracketed paste cannot execute
shortcuts. This does not emulate the remote application's keyboard mode stack.

Interactive nested `spawn` commands ask
the existing daemon and client to prepare a child attachment, then exit only
after the child is ready. Preserve the original owner PID and creation identity;
the short-lived launcher and terminal client never become its lifetime owner.
Drain parent input before switching. Detach and child exit unwind one level;
failed startup restores the parent and reports failure to its launcher.
Interrupts during authentication or setup must cancel the child connection and
restore the parent; signal handling stays active through attachment transitions.
After attachment, OS interrupts are queued as Ctrl+C terminal input instead of
cancelling the client transport. Ctrl+C while a switch is pending cancels its
handoff and returns through the detach barrier to the original attachment.
Repeated interrupts during parent recovery do not unwind additional levels;
SIGTERM still cancels the active attempt. Initial connection interrupts cancel
startup normally. Signal handlers remain installed until keyboard shutdown and
console restoration finish. Confirmed exits carry the host's reason to the client;
exit and unexpected-disconnection notices print after console restoration, also
when returning to a parent attachment. A transport failure reports its category
and client exit code without asserting that the hosted shell ended.
Connection boundary cleanup disables Kitty keyboard
event reporting and xterm modifyOtherKeys before returning to the local prompt.
The Linux account daemon catches SIGHUP for its entire lifetime, including
intervals without hosted shells; child shells retain their normal HUP behavior.
The frontend holds local daemon leases for the active route, the source of a
pending switch and the parent return stack. A child adds its parent to the stack
once startup succeeds. The frontend then closes unreferenced leases.
Each lease can open independent attachment streams
over the daemon-owned SSH connection. Only the interactive frontend retains
leases across attempts. A fresh dial for a saved route or control request
belongs to its stream.

The shell host holds a terminal and the verified process identity used for task
operations. Each attachment borrows one input lease; the host stops that writer
before admitting another attachment. Every attachment opens a separate raw
terminal endpoint. For direct remote launch and attach, each endpoint uses a
native SSH `direct-tcpip` channel to the authenticated daemon. Local attachments
open separate daemon connections. Spawn lifetime transports and reverse routes
open separate endpoint streams through their authenticated relays. A single-use
token scoped to that shell pairs the terminal endpoint with its control
attachment, expires after five seconds, and is bounded to eight pending endpoints.
The endpoint carries raw terminal bytes after its handshake. Resize and
approval messages stay on the control connection. Input byte counts order detach
after accepted input; exit includes an output byte count so the frontend drains
the tail before reporting the exit status. Closing an attachment releases its
channels and input lease, preserving the hosted shell and reusable SSH connection.

The attachment carries control and approval messages on one connection.
Each control message is a newline-terminated JSON value of at most 68 KiB
before the delimiter. The mount agents use bounded JSON lines
for their own messages. Peers must use matching full releases. The readers
retain any bytes buffered during a handshake when the connection switches to
a raw protocol.

The local frontend-to-daemon lease uses smux to open independent endpoint
streams over its shared connection, with 32 KiB maximum frames to reduce local
bulk-transfer overhead. Indirect forward muxes use 16 KiB frames to retain finer
interleaving on network transports. Each SSH connection uses native channels
over a single underlying TCP connection. Indirect forwarding uses its own smux
relay.

For a nested `spawn` initiated inside an attached shell, the initiating host's
daemon uses that shell's configuration, signing credentials, environment and
working directory. `ProxyJump` uses SSH forwarding through its jump hosts
and requires the managed shell daemon only at the destination. With nested
`spawn`, the terminal client follows
a private attachment route, not remote-supplied local execution instructions.
The first attachment confirmation consumes only the startup reply slot. The
private route stays bound to that child task instance until its owner lease
ends. For each route request, the daemon opens an authenticated endpoint to
that instance. Same-host spawns use a local daemon connection. Next-hop spawns
open a native channel on the daemon's existing SSH connection and repeat the
same process at each hop. The daemon relays endpoint bytes without interpreting
terminal or file traffic. Both endpoints of a routed sibling attachment bind to
the selected shell instance, including after a rename.
Local managed shells also support handoff for remote `launch` and `attach`.
A native shell reports live handoff destinations in list details and pstree
while the frontend holds the observation stream. Attachment remains exclusive
per shell, and a detached return target is eligible for ordinary attach.
If a return target is unavailable, the frontend reports the failed return and
exits after releasing the departed attachment.
Detached startup keeps its noninteractive path. Reuse the existing connection and
attachment mechanisms; do not run a second terminal frontend inside a daemon.
Native and hosted shells release the input lease through the same control
loop. It confirms detach only after the active attachment releases its lease,
then waits for `bye`. On exit it closes the transport and joins its input
reader even if the reply fails.
The frontend waits separately for queued input to drain and for the detach
confirmation. The control loop owns both deadlines and joins the terminal
writer on exit. Once detach has been sent, a failure on the input stream leaves
the control stream available to receive that confirmation.
If input does not drain in time, the host leaves the barrier unacknowledged and
continues reading control so the frontend can detach with an uncertainty warning.
If a local handoff is prepared but its commit does not arrive within five
seconds, the frontend resumes input on the current attachment and reports the
timeout to the requesting daemon.
The frontend keeps reply deadlines with the pending request. Once it has a
target, it queues detach with the switch and uses the same switch to continue
after the detach confirmation.
The daemon accepts one preparation reply from the attachment that received the
handoff offer and one startup result in the commit phase. A duplicate reply
cannot satisfy the next phase.

Local handoff lives in `client_connection.go`; remote child ownership and routing in
`spawn_route_linux.go`; the frontend return stack and connection restoration in
`client_connection.go`. Reattachment and review must preserve the route and its
parent instance. If it expires or changes, fail rather than reinterpret the
child's host alias as a direct destination. See the
[handoff trust boundary](SECURITY.md#trust-boundaries).

Commands select tasks by name or short ID. A bare kill selector and
`self:ID` are restricted to tasks managed on this machine. Remote kill
selectors require the host in `HOST:ID`, including for cached rows. The list
displays the first six characters of each task UID and orders tasks by full UID
within each owner group. The process tree sorts roots and siblings by short
ID so attachment and observation updates do not reorder them. Short-ID operations reuse the
connection settings of their observation. Confirmed detach acknowledgements
(including Ctrl+B d, helper detach, switching, and --detach startup) promote the
immutable shell identity in one bounded, daemon-serialized MRU stored in
`connections/detached.json`. Global attach considers that entire
order; HOST filters it to one remote host before choosing a still-detached
candidate. Local shells can be selected directly by ID or with `self:ID`. The account daemon resolves a native shell and opens its control stream
in the same authenticated request. Detach and handoff from a native shell use
its registered identity directly. A hosted shell chosen from the startup menu
is attached by the UID captured when the menu was shown. The next, previous and
last shortcuts also attach by UID. Last remembers the instance across a rename.
Unique ID prefixes of 2–32 hexadecimal characters resolve exactly;
ambiguous prefixes fail without choosing a candidate. When an operand can be
an ID prefix or an alias, both checks use the same task observations.
Native and hosted shell renames resolve the ID and current label together.
The daemon rechecks the selected task and label while claiming the new name
under the task-table lock. A single attach operand
must name a known task or a host in the daemon’s shell observations; unknown
targets fail before SSH connection setup. HOST NAME is an explicit remote request.
The daemon checks remote detach candidates against shell history in the same
request. Selection retains the saved route and exact instance, using the latest
confirmed label. The daemon holds saved routes read-only, so shell snapshots
from one inventory can share route ancestors in memory. Local candidates
resolve against live registered tasks.
Inventory refreshes never promote entries. Legacy connection history is a
fallback when no eligible MRU entry is available. The daemon checks saved shell
routes and recent host defaults under one lock and returns the selected
connection. After a confirmed remote rename, one metadata operation updates
the shell snapshot and the recent connection label. When the command selected
an instance, both updates match that instance. Scoped history operations
collect matching snapshots from the same
scan that expires old records. Explicit names and IDs bypass MRU selection;
an actual later detach updates it.
Actions revalidate immutable
instances, capabilities or process identities at their authority. Displayed process
ancestry is not permission to signal. Follow the
[shell control safeguards](SECURITY.md#shell-control-and-lifetime).

### Account daemon and mounts

One `sshore daemon` per OS user account owns native terminals, hosted Unix shells
and resources in one in-memory task table. That table owns identity,
lifecycle and live connection relationships; it is never reconstructed after a
crash. The loopback TCP listener serves HTTP/1.1. Control requests use JSON
with an account token and exact release version. The same token authenticates
administrative snapshot and shutdown requests across releases that speak HTTP.
Linux hosted shell creation returns after task registration. Nested routes on
the same daemon resolve names and register children directly in the task table.
Interactive task and lease connections and multi-step startup use HTTP Upgrade.
Upgraded connections carry the existing task streams, including smux where
the session uses multiple streams.
Only endpoint discovery and credentials need private daemon files.
Every normal public command registers with this daemon after argument parsing;
help, version output and diagnostics remain free of startup side
effects. The daemon's single one-second clock checks external spawn-owner process
identities, broadcasts a shared lease heartbeat every 20 seconds and reclaims idle daemons.
Each tick reads a given owner PID once and compares its creation identity
against all leases watching that PID.
In-process and socket leases share the same registration checks. The daemon
retains only the data needed by each live lease. An incoming lease owns a copy
of its peer metadata, and owner leases require a process identity. Socket
leases expire after 75 seconds without acknowledgement. The
daemon's reader removes a lease on EOF, timeout or explicit release, even if a heartbeat
write is blocked. Client cancellation, reader exit and explicit release share
one stream close. Explicit release waits briefly for acknowledgement and joins
the client reader after closing the stream. Tasks and live leases
prevent idle exit. After the last activity, the normal idle grace is 30 seconds.
Idle exit, reviewed shutdown and listener failure share the same cleanup for
sockets still reading initial HTTP headers. Admitted HTTP requests get a
bounded drain, and the daemon joins its activity clock before releasing the
owner lock.
The HTTP upgrade hands over a stream only after validating the protocol and
full release and checking that the handshake context is still active.

Peer advertisements, incoming-resource participation and client attachment leases
live in daemon memory. Expired advertisements are removed during listing, on
clock ticks, or before rejecting a new advertisement for lack of space. A matching
live lease keeps the displayed record fresh while that lease remains active.
Each inventory request returns either the peer view or the incoming resources
backed by live leases.
Task trees, remote shell observations and recent connection
routes are loaded once from private history and then served from daemon memory;
only the daemon persists changes. Recent connection and detached attachment
history retain their own order and use the same bounded file handling. History
loading resolves the state root once and prepares directories under that root.
Recent-history updates validate the directory of the loaded file before replacing
it, then publish the new rows in memory. Saved
shell routes are validated when loaded or updated. A saved spawn route is
ambiguous when multiple instances share its host and name. Select by task ID
in that case. The daemon reconciles shell
history against each task-tree record's owning node and prunes expired rows
under lock. Detached attachment lookup keeps only observations for identities
in its recent list. Remote inventory submits shell observations and the query's
error state to the history cache. Listings sort the returned rows, while cache
updates use them directly. All task-tree writes use the same size limits as
reads. Loaded relationships are cached observations. The task registry supplies
live tasks. Clients do not poll files, hold liveness locks or
publish advertisements through SFTP. Forward metadata uses the same daemon clock;
mount metadata is registered with its existing agent connection. UI progress,
terminal mechanics, SSH protocol keepalives and filesystem cache deadlines retain
their own timers because they do not establish product task liveness.

Its private owner lock is independent of
client runtime directories. While it is running, its explicit application state
root is authoritative; a conflicting `SSHORE_STATE_DIR` is rejected. Test builds
use isolated account roots. The daemon supervises mounts through inherited pipes
to a separate `sshore daemon mount` process, which owns the native filesystem
work. The daemon runs forwards directly and handles their control requests in
process. The launcher derives the resource kind from its configuration, and
the daemon checks that it matches the requested startup kind. Reviewed mounts
and forwards are validated before selecting a task name and consuming approval.
Both use the same startup commit exchange and keep their own SSH
transports. The daemon keeps a committed task if the initiator loses the
final acknowledgement. The mount worker owns its commit reader and unmount
requests. The forward's caller owns its commit reader and acknowledges only the first ready
connection. The daemon registers the mount before starting its worker, so the
task name is claimed during setup. Startup updates the registered record's
runtime details and reports the identity and name held by the daemon.
An initiator disconnect and a daemon stop share the same mount startup
cancellation, which closes worker input. Cancellation and the handoff to task
control use the same lock. Startup owns both pipes through the configuration
write, the commit write and the worker's acknowledgement.
The daemon then uses them for task control and can read the final reply even if
the worker has already exited.

The daemon checks proposed names and selects automatic names from the task ID's
prefixes. Creation and rename use the same name check under the table lock.
Registering a review also checks pending request capacity in that scan. Table scans
discard expired entries with a single time reference. Lookup, name checks and
batch confirmation within a task-table operation use the same reference time.
Batch stop resolves each confirmed task ID directly and checks its displayed
reference and kind using the same check as single-task operations. It verifies
the whole batch before removing pending requests or stopping workers.
Registration checks expiry after acquiring the table lock. Request reads copy
the stored connection and operations after releasing the table lock. Inventory
collects task references under the lock and reads their details after releasing
it. Shell rename writes history after releasing the table and shell
state locks. Renames of the same shell write history in live update order.
Native and hosted shell state come directly from the account
daemon. Native shell selection uses a direct lookup for a confirmed UID and
copies the selected record after resolving an alias or prefix. Hosted shell
selection resolves one task in the daemon's table without
collecting process diagnostics or sending the full shell inventory.
Local shortcuts, task ID lookup and detached-shell selection use the same
native and hosted shell rows. They use the hosted shell health query, which
skips working-directory and process diagnostics. The local list also reads
working directories.
Detailed listings share one account-owned process snapshot across hosted shells.
Resource cancellation and rename resolve the target in the
daemon task table. A target selected from inventory must still have the same
identity and label. The resource inventory includes pending request summaries
from the same task table as mounts and forwards. The daemon selects matching
resources during the table scan and reads worker details after releasing the
lock. It sends resource records first, then streams completed status probes
from the same selection.
A stalled worker leaves its record visible alongside any statuses already
received. The client applies each probe to the resource row for that identity.
Task selection uses the same rows without waiting for probes. Batch shutdown
passes the confirmed rows' names and identities back to the daemon for rechecking.
Request payloads remain in
daemon memory until expiry or single consumption. If the shell fails to confirm that it notified the request
helper, the client cancels the saved request by ID. The shell publishes its
advisory row after notifying the helper and removes it if it cannot acknowledge
the client. Local selections exist only in the review process. An approved upload derives its transfer
metadata from the same open file handle that supplies its contents.
Review does not attach a terminal.

Updates query the authenticated administrative endpoint, compare the confirmed
task snapshot and request shutdown after preparing the replacement. A refusal,
unreachable endpoint or timeout stops the update. There is no daemon process
scan or forced shutdown fallback. Administrative inspection/shutdown is separate
from exact-release normal control and cannot start work or open task streams.
State replacement and executable staging share the same temporary-file
lifecycle. The file is synced and closed on the destination filesystem before
the caller takes ownership. Installation verifies the staged version before
stopping tasks.
The daemon verifies confirmed task IDs, names and expiration by direct lookup.
It scans for expired extras only when the task table has more entries than the
snapshot. Pending requests are removed after the entire snapshot is verified.

Spawn bridges open an independent local data connection for each stream. Mux
buffers remain bounded, and authentication admission applies backpressure
instead of dropping valid connection bursts.

Startup must have an explicit ownership handoff. Carry the initiating client's
working directory and environment, not whichever environment started the daemon.
The launcher bounds startup pipe I/O and verifies the daemon's published record
before confirming readiness. If the handshake fails, it reaps the child before
releasing the startup lock.
The mount CLI and approval flow send the same structured settings. The worker
validates those settings and the approval route before preparing the mountpoint.
Approved resource routes acknowledge readiness after checking the source shell
and reserving any required remote listener. Approved uploads check the pinned
local source before opening a transfer stream.
Do not hold bookkeeping locks across terminal I/O, authentication or worker startup.
Daemon liveness comes from its owner lock; orphaned metadata cannot block startup
with a version mismatch. `kill --all` uses authenticated administration across
releases that speak HTTP. Under the task-table lock, the daemon expires stale
records, checks that the reviewed task count matches, and verifies each ID and
name directly against the table before collecting the workers to stop. It
then stops those workers and their identified processes and releases its account
endpoint. The client polls one validated handle to the account lock throughout
shutdown. On Windows it releases that handle before waiting for the daemon
process to exit so the installer can replace its executable. Whole-daemon shutdown owns the
cleanup deadline. The shutdown request waits for each accepted stop. On timeout,
that wait continues in the background. Admission resumes after an error or
timeout once cleanup finishes. After remote shutdown succeeds, host-scoped `kill --all`
rechecks the entire displayed set of local resources in one operation before
starting local cleanup.
Cancellation must close owned resources and join background work before reporting
completion; persisted PIDs are diagnostic, not authority to terminate.

Keep permission and transport separate. Direct commands are local grants; remote
requests require separate local review before execution. Copy preflight creates
nothing, approval consumption is single-use, and file publication must not overwrite.
State-file replacement cannot substitute for those command-specific guarantees.
See [approval safeguards](SECURITY.md#local-approval).

Copy and mount helpers use the shared preparation path before starting.
The application owns the direct forward listener, workers and SSH connection
as one lifetime. `internal/ssh` provides channels and knows no forwarding rules;
`internal/forward` owns byte relay and multiplexing. `ssh -G` resolves connection settings only; configuration forwarding
rules are cleared and cannot add listeners to the product-owned rule.

Ordinary forwarding, inventory and cancellation do not install remote software.
File and SSH behavior live in `internal/files` and `internal/ssh`. Account setup
and consent stay in the application.

Client transport openers return an owned `net.Conn`, making deadline support
explicit rather than asserting it at runtime. `Close` releases its entire
owned path, and context cancellation invokes the same cleanup exactly once. A
reused attachment owns only its stream; the frontend retains the authenticated
connection across attachment changes. Resource routing checks the full release
version and requires a shell instance. Approval checks the instance under the
same lock as the shell name, process and attachment state. File and mount routes
can acknowledge readiness after approval. A reverse forward binds its listener
before acknowledging readiness.

SSH `Start` owns connection cancellation; `Client.Start` opens a session on a
caller-owned connection. Both cache the exit result for concurrent waiters. Direct copies and mounts use that stream directly.
`CloseWrite` sends channel EOF while leaving output available. `CloseAndWait`
aborts before joining, while normal completion consumes output before `Wait`.

The SSH authentication broker uses a shared, bounded loopback connection pool
without proxies or redirects. Its loopback endpoint and token travel as
private startup fields outside the subprocess environment. Dialing is bounded
independently of interactive input. Visible prompts feed the line editor through
the cancellable keyboard reader. Finishing a prompt cancels that reader, closes
the input pipe and waits for the reader to exit before restoring the terminal.
Closing the broker cancels requests and waits for terminal restoration.
Background forwards use their task registration for daemon liveness and reuse
one heartbeat advertisement across connection attempts. Each attempt starts
heartbeats after readiness and preserves write failures as its cancellation
cause. It waits for the transport once and joins connection cleanup and any
heartbeat work before retrying. User cancellation remains a normal exit.
The remote watcher reads records directly from a routed control stream. The
standalone SSH helper adapts inherited stdin so signals can release its lease
even while stdin is blocked.
The daemon cancels a forward directly. Mount workers return the result of a
graceful unmount so the daemon can report a busy mount while it remains
connected. Single stops, batch stops and daemon shutdown share the same
worker stop operation. Once a stop is accepted, concurrent callers wait
independently for the same task to exit. A refused stop can be retried.
Shell resource routes validate the session identity and approval, then hand the
connection to the resource service. Each approved file opens a separate endpoint
that rechecks the source identity and release before its SFTP handshake. Mount
data and notifications use separate authorized endpoints. Forwarding owns its multiplexing and relay lifecycle in
`internal/forward`; its public endpoint returns ordinary I/O interfaces.

SSH and resource mux forwarding share a lifecycle that cancels pending dials and
relays, closes the inputs, and waits for workers when accepting ends or the owner
stops the forward.

Shared relays distinguish opaque attachment bridges (EOF closes both directions)
from request/response splices (clean EOF sends `CloseWrite`, preserving replies;
I/O errors and cancellation abort both ends). Both join their copy workers.
Full shutdown starts closing both endpoints before waiting for either protocol
close to finish. Both ends of a routed forward establish the control stream
before starting data forwarding. The forwarder then takes over the mux and
the optional listener.
Forward attempts share one transport close across cancellation, handshake
failure and normal cleanup.
The listening side opens each data stream and watches for mux failures or
unexpected incoming streams even while idle. The dialing side accepts data
streams and connects to the local target. Shutdown joins both the accept loop
and all forwarding workers, including the listener's mux watcher.

Copy concurrency must stay bounded and preserve exact-size checks, ordered digest
calculation, receiver-side upload verification and no-overwrite publication.
Direct copy resolves all paths, validates remote upload filenames and opens
local sources during batch preparation. The destination picker closes before
execution begins. Destination selection and local staging both check for
existing entries, including dangling symlinks. Direct and approved copies
share a sequential batch executor that closes each completed source before
advancing and stops on the first failure. Review keeps the original request
with each prepared copy and assigns the approved stream's virtual paths
during local selection.
Downloads and verified uploads publish through the same size check, file sync
and hard link. Both check cancellation after syncing and before creating the
link. Upload verification also checks cancellation when the file is empty.
Direct uploads resolve publication against the staging file's directory.
Approved uploads publish to the destination selected for that stream.
The receiver removes its temporary upload during publication, on a cancellation
request or when the stream closes. Successful removal releases the slot for
direct copies. An approved stream reserves its single slot until it closes.
If removal fails, cleanup can retry while the upload remains closed to writes
and publication.
Each approved transfer negotiates one SFTP operation on its own endpoint.
The batch retains its authenticated SSH lease and closes it after the last
transfer or cancellation.

Mounts always use the remote sshore helper and the same setup/version safeguards as
`cp`; there is no plain SFTP compatibility path.

Mount failures can leave writes uncertain; never replay writes implicitly.
If exclusive creation reports a collision or generic SFTP v3 failure, a
nonexclusive open can recover by checking for an existing regular file and
opening it without creation.
The SFTP timeout checker scans the bounded set of outstanding requests at timer
wakeups or idle transitions. The initial SFTP version exchange uses the same
request timeout after SSH authentication. Replies remove requests without
updating a second ordering structure. Mount cleanup uses the mount context to
stop the timeout checker and joins it alongside the SFTP disconnect watcher.
The SFTP wire creates directory budgets on the first read of each handle.
An oversized listing counts as a failed request and leaves the connection
available for other operations.
Keep caches and notifications bounded and advisory. Invalidate affected paths
and parents on mutation; notification loss or unknown scope requires full
invalidation. The bounded refresh queue keeps entries in access order and
resumes pending work when mutations finish. Each refresh refers to a specific
cache entry. Removing an entry releases its payload immediately. The scheduler
discards the detached node when it next scans the queue. Invalidation also
retires matching in-flight lookups. A lookup can store its result only while
its request is still registered, and a refresh can update only the entry it read.
Lookups affected by an in-progress mutation go directly to the server. They
skip the shared cache and in-flight registry and leave parent recency alone.
Foreground reads and background verification use the same query and freshness
rules, including how directory listings seed child attributes. Each cache key
carries the query kind and remote path, or the open handle and its parent
directory. Foreground lookups derive the watch and parent recency from that
key. A directory listing watches the listed directory itself.
The refresh scheduler drops expired work and selects the newest eligible entry
in one pass. It reuses one timer for debounce and rate limits.
Background verification uses its own context and
must not delay foreground reads. A cancelled waiter leaves the
shared query available to other readers.
Foreground lookups and prefetch share a bounded directory subscription queue.
Foreground lookups proceed immediately. A single timer bounds each serial
registration. The stream reader handles acknowledgements in order with dirty
and reset messages. An acknowledgement received with no registration in
progress disables notifications. Each queued directory has one completion
signal shared by the registration worker and any prefetch waiting to read it.
Prefetch can wait for registration to avoid having its listing discarded by
the acknowledgement.
The Linux agent merges filesystem events by directory in a bounded batch. Its
collector keeps draining events while the writer is busy and hands off each
batch when the writer can accept it. Overflow sends a full reset. The agent
ends notifications when its filesystem watcher stops and joins both the request
reader and event collector before returning. The client
invalidates its cache before releasing registration waiters through a shared
signal. A direct mount owns one SSH connection and opens separate exec channels
for the SFTP subprocess and notifications. Routed mounts open separate
authenticated endpoints for data and notifications and validate the source shell
identity and approval on both. If the notification endpoint is unavailable, the
mount falls back to short-lived metadata caches. The mount agent waits for its
SFTP subprocess directly. After the subprocess exits, the agent gives final
replies up to one second to drain, then closes the data endpoint and joins its
workers.
For a background mount, startup cancellation closes the worker's input. The
daemon starts its worker-exit deadline before waiting for final control replies.
The worker uses one reader through startup and control, with a size limit
on each JSON record.
On the client, one startup watcher handles the deadline, interruption and
initiator disconnect. Those events, request timeouts and transport loss share
a cancellation path that retains the first failure and closes the owned byte
stream. Shutdown closes that stream before joining background cache work or
closing the SFTP client, so blocked requests cannot hold up cleanup. The mount
joins the startup watcher before reporting readiness.
A failed startup or lost connection closes the transport and gives unmount up
to ten seconds to finish. If the transport fails during a graceful unmount,
the same attempt gets up to five seconds to finish after transport shutdown.
The SFTP request watchdog sleeps while the wire is idle and reads the oldest
outstanding request directly from the wire's request table.
Linux mounts use 64 KiB SFTP read and write chunks with the OpenSSH mount agent.
Mounts learn from successful foreground directory listings. Per remote account
and resolved root, they remember up to 1,024 relative paths, halve visit weight
every seven days and forget paths after 30 days without a visit. After a
foreground listing, mounts prefetch all direct child directories when there
are at most ten, excluding dot-prefixed entries and symbolic links from both the
count and the candidates. In wider directories they fetch up to ten children
with positive visit weights, highest first. Up to four workers take the ranked
targets in order and fetch listings through the shared cache.
Navigation publishes a batch with its own targets and context. It cancels the
previous batch and replaces any queued work. The worker waits for the current
batch to finish before it takes the replacement.
Prefetch never trains history or walks descendants. Mount shutdown cancels and
joins both background workers before merging visit counts into private local
state.
Foreground entries and unused prefetches each keep their own recency order.
Eviction takes the oldest unused prefetch first. A foreground listing moves
a prefetched entry to the front of the foreground list and seeds its file
attributes within their original freshness window. A foreground request that joins an
in-flight prefetch gives it foreground priority at publication. Foreground
metadata lookups also touch the cached foreground parent listing. Recency
affects eviction order without extending freshness deadlines. Background
verification considers only foreground entries.
Scheduling and cache behavior belong in `internal/files/` and its tests.

Windows mount metadata-only opens reuse bounded, short-lived SFTP handles in
`internal/files/mount_metadata_windows.go`. Data and mutation opens get fresh
handles. Handle retention, attribute freshness and capacity probes have
separate lifetimes. One timer expires reusable handles based on when they
opened. Active leases still refer to the original pool entry and its handle.
The last release of a retired entry, or pool shutdown, removes that entry
from the ownership table before closing the handle. Shutdown waits for
pending opens to close any handles they obtain.
Local mutations and remote invalidation advance the handle reuse epoch.
The pool checks the epoch and replaces stale entries under the same lock.
Expiry and unmount close idle handles. Both platforms cache volume capacity and
probe failures by remote path and invalidate them on mutations. Each directory
handle has its own cursor over an immutable cached listing. Linux translates
entries as FUSE reads them. These caches do not buffer file writes.
Both mount adapters use the same attribute update and cache invalidation path.
Partial SFTP time updates preserve the omitted timestamp. Supplying both
timestamps avoids a read to fill in missing attributes.
Linux validates path components before normalization and reuses the resolved
path when reporting attributes after a change.

### Inventory and diagnostics

Separate live observations, cached records and unknown state.
The shell rows in `pstree` come from registered local shells. Saved snapshots
supply ownership and attachment context for those rows. Available shell rows
take precedence over unavailable copies. Among equal-priority rows, newer
timestamps take precedence. Saved evidence with equal priority and timestamps
is resolved in filename order.
A spawn without a visible owner task may display its original direct parent
process, using its saved PID and creation identity. Tree history copies that
display data when it enters or leaves the daemon. Internal updates can share
the read-only copy. Reattachment preserves this context. Only a matching local
process identity supplies a live
name. Foreign processes are not probed in the local PID namespace. These external nodes are
rendering context, not registered tasks or additional process ancestors.
The renderer indexes visible tasks and external parents together and creates
display edges without rewriting ownership records.
`pstree` cuts a display edge at the lowest identity in each ownership cycle so
inventory order cannot change which node becomes the root. It groups rows by
their display owner in the same pass that marks the current shell's ancestry.
Resources from one inventory share a timestamp, so equal-freshness rows sort
by kind and name.
These relationships
never authorize operations. Task IDs are allocated at creation;
alias changes preserve the instance and storage identity. Prefix collisions at creation
require a new identity; imported ambiguous references require an explicit scope.
Selection checks ID prefixes before treating a hexadecimal reference as an
alias. It keeps one candidate per identity. Registered local shells take
precedence, and a live remote observation takes precedence over an ended
record for the same identity. A local-shell alias lookup reads only local
shell sources.
Keep allocation and reference policy in `task_identity.go`, separate from capabilities.
Spawn owner context stays fixed;
the current attachment source may change. Neither is verified OS process ancestry.

Keep normal output compact and distinguish cached, live and unknown state.
Closing a remote spawn's owning transport retires its cached instance.
A complete inventory also retires an absent spawn from the same remote account.
The default list reports uncertain cached attachments as unconfirmed.
The local and background inventories match a live local resource's peer
advertisement by UID when available. Advertisements without a UID use the
manager and resource label. Exit receipts do not hide peer advertisements.
The account daemon filters local resources by host and captures their names
in one pass under the task table lock. A request for status also captures the
workers, then streams their results by UID outside that lock.
Pending reviews have no worker to probe.
Full diagnostics belong in `list --json`. The local JSON view and remote
inventory endpoint add shell exit receipts and saved tree context to the live
inventory. The attachment shortcut streams task rows and errors directly
from the live inventory. `pstree` reads saved relationships for its live rows.
Resource history reads at most the 100 newest receipts per kind from the last
seven days, matching on-disk pruning.
Shell exit history and resource receipts share these limits. Entries with equal
retention timestamps follow filename order.
Exit receipts retain the task UID across renames. New receipts store the
final alias in the resource record. When a mount worker exits, the account
daemon applies any later alias without repeating history pruning. The reader
also accepts older receipts with a separate display name.
Incoming resources require a live
local transport socket lease, not merely a historical advertisement. Worker liveness
is not target health. Exclude capabilities and credentials from public output.
See [inventory privacy](SECURITY.md#inventory-and-privacy).

### Setup and installation

Local installation runs the bundled platform installer. With a host, `install` sends the
bundled Linux installer over SSH and pins the release to the client's exact
version. The target runs the installer and downloads the release. Installers
verify the checksum and call the prepared binary to apply the installation.
The installer stops the target daemon before replacement and prompts for `kill`
when it owns tasks. Remote confirmation travels over the authenticated SSH channel.

The embedded `scripts/remote.sh.tmpl` checks the release and dispatches normal
connections to their internal service entry points. SSH stdin remains available
for service protocol frames.
[SECURITY.md](SECURITY.md#remote-setup-and-updates) owns the installation safeguards.

## Build and validate

Use the shared toolchain on the normal PATH; check locations and versions against
repository requirements before development. Do not lower requirements or install
private runtimes. For Node, use system `node` and global Corepack, letting the
repository's `packageManager` select the version. Keep shims in the configured
global npm prefix and use standard caches. An incompatible global toolchain needs
user authorization before updating it.

For browser checks, reuse global Playwright at
`/home/jay-waves/.local/lib/node_modules/playwright` and system Chrome with
`chromium.launch({ channel: "chrome" })`. Do not add project dependencies or
download browsers or reinstall Playwright without approval.

```sh
command -v go
go version  # Go 1.26 or newer
go build -o dist/sshore .
```

Use `WaitGroup.Go` for scoped worker launches and `OnceValue` for immutable
one-time results. Keep explicit task accounting where registration under a lock
or completion in another function is part of the lifecycle contract. Pure
concurrency and timer tests use `testing/synctest` with in-memory I/O; real TCP,
PTY and subprocess tests stay outside its virtual-time bubble.

Choose checks for the actual impact and risk. The table suggests options, not a
mandatory sequence: use targeted checks when sufficient, and skip checks that
would not add useful evidence. Do not wait for a full pipeline merely for
completeness.

| Change | Validation |
| --- | --- |
| One module or command | Relevant existing tests with `-run` |
| Cross-component behavior | Target affected boundaries; use the core suite when impact is broad |
| Concurrency or shared lifecycle | Relevant tests; consider `-race` for shared-state changes |
| SSH transport or remote setup | Relevant tests; use focused real-SSH checks for connection behavior |
| Native terminal or mount behavior | Relevant native platform checks; compilation alone is insufficient |
| Installation or packaging | Relevant installer or release tests |
| Documentation | Relative links/anchors, code fences and affected Mermaid diagrams, including packaged documents |

```sh
go test -count=1 -timeout=5m ./...
```

Subprocess builds inherit `GOFLAGS`; use `GOFLAGS=-race` for race checks and
`-count=1` for environment-dependent checks. Reuse successful checks unless a
subsequent change or unresolved failure warrants repeating them.

Go lifecycle fixtures re-execute the test binary with temporary daemon and cache
paths. Use a normal build when the fixture isolates daemon ownership with OS accounts.
Use `tests/testutil/sshtest` for Go workflows that need an authenticated
loopback SSH connection. Tests provide the exec or forwarding behavior they
exercise. Configuration-only checks can observe the SSH configuration command,
and local-only workflows use the daemon or the terminal fixture directly.
The external CLI and real-SSH scenarios live under `tests/integration/`.
For local real-SSH checks on Linux amd64, run the Python fixture through Docker.
The runner uses the host Go toolchain to build the normal binary. Each scenario
gets a fresh container with separate client and server accounts, an isolated
loopback network and read-only test artifacts. The first run builds the test image.
The setup scenario also checks that installation rejects a running or idle daemon
and that reset clears only application data.

```sh
tests/integration/ssh_docker.sh
tests/integration/ssh_docker.sh --setup-only
```

Native mount smoke tests are opt-in and require an existing FUSE/WinFsp environment:

```sh
SSHORE_TEST_FTP_MOUNT=1 go test -count=1 -run '^TestFTPNativeMount$' ./internal/files
```

The manually triggered Tests workflow is the unified Linux/Windows amd64
validation pipeline. Choose the workflow ref, or supply a release tag or commit
in its optional `ref` input. Pushes, tags and publication never trigger it;
validation is independent of CI/CD and can also run locally using the commands
above and the installer/release scripts in `tests/`.
Linux runs the Go suite with race detection, installer and release checks,
and all containerized SSH scenarios. Windows checks the build, platform adapters
and both supported PowerShell installers. Linux installs and verifies Bash, Zsh
and Fish for the Go suite. Go failures and skips are
listed in the workflow summary; complete Go results are retained as artifacts.
Native FUSE/WinFsp checks remain opt-in on equipped hosts. Other environment-based
skips must be reviewed; a green run is not proof that skipped checks passed.
The Docker runner defaults to all scenarios, each with an isolated fixture; use
`--core-only`, `--spawn-only`, `--identity-only`, `--nested-only`, `--handoff-only`,
or `--setup-only` for a focused rerun.
Local in-process benchmarks are not WAN performance evidence.
Benchmarks live under `tests/benchmarks/` and can be run separately with
`go test ./tests/benchmarks -run '^$' -bench .`; use `-bench` to select a narrower
workload when comparing changes. The smux benchmark fixes its own limits to
compare frame sizes; it does not check production configuration.

### Shell and terminal validation

Supported targets are listed in the [README matrix](README.md#os-and-shell-support).
PowerShell interactive validation uses PSReadLine.
Existing tests are evidence for their covered behavior, not a fully validated
terminal/shell matrix. User reports and cross-compilation do not replace native
interactive checks.

For terminal changes, check pending input/cursor retention, redraw, interrupt and
job control across detach/reattach, fragmented escape sequences, paste boundaries
and cleanup before returning to local prompts. Preserve platform/build-tag
isolation. Enhanced keyboard protocols, custom prompts and terminal capabilities
can affect behavior; record meaningful regressions as focused tests.

## Release

1. Keep `VERSION` and the source default in `main.go` equal to the new full release
   version. Update the notes block in `scripts/release.sh` with the new version's
   changes and migration consequences. Earlier notes are available in Git history.
2. Run checks appropriate to the changes, then commit and push. A push to `main`
   triggers Linux/Windows amd64 builds and packaging; successful builds permit
   publication only while that commit remains the branch head.
3. After publication, verify the release, commit/tag identity, both platform archives
   and checksums. Then remove older releases in the same series, retaining their
   Git tags. Full validation remains independent of publication.

Published tags and assets are immutable. Source-only edits at an already published
version do not update its archives. Retain only the highest published patch in each
major/minor series after verifying complete replacement assets; never delete Git
tags or prune on failed/incomplete publication. Drafts, prereleases and noncanonical
tags are excluded. Packaging and publication live in `scripts/package.py` and
`scripts/release.sh`; the publisher performs retention after verification.

## Planned work

`pull` / `push` directory synchronization is not implemented. The current proposal
is one-shot by default, optional background watching, explicit opt-in to propagated
deletions, and conflict reporting for independent edits. Revisit the design before
implementation; it is not an existing command contract.

`daemon_metadata.go` owns serialized task-tree, shell-history and recent-route changes; `daemon_activity.go` owns live socket leases, peer advertisements and the shared heartbeat clock; `reset.go` coordinates authenticated daemon shutdown and state removal; `state.go` owns account display identity and remembered connection entry points without depending on CLI options. Native OS identity reads live in the platform adapters. Reset ends registered tasks and clears application state; coordination locks remain to serialize startup.
