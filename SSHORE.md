# sshore development

This is the development document. Read [AGENTS.md](AGENTS.md) before changing
code; use [GLOSSARY.md](GLOSSARY.md) for terminology and
[SECURITY.md](SECURITY.md) for security decisions. The [README](README.md) is the
short user manual. CLI help is the executable command reference.

## Architecture and code map

sshore is a Go CLI using system OpenSSH for authentication and transport. Remote
persistent shells run on Linux; local clients and resource workers support Linux
and Windows on amd64 only. Mounts and independent forwards do not share a shell's connection or smux streams.

| Area | Entry points and responsibility |
| --- | --- |
| CLI | `main.go`: executable entry point; `cli.go`: shared argument scanner, dispatch and session-management parsing; `cli_{shell,cp,mount,forward,endpoints,ssh}.go`: command grammars and SSH-only suffix; `cli_shell.go` also coordinates shell command dispatch; `cli_help.go`: command help |
| Connection and setup | `client_connection.go`: local/SSH connection ownership, session switching and reconnect fallback; `client.go`: shared interactive loop; `remote_bootstrap.go` and `setup*.go`: shared remote preparation, downloads, update confirmation and recovery |
| Shell | `internal/shell/`: shell startup options/environment, attachment I/O, muting, input barriers, exit draining and detach protocol; root `launch_*_linux.go` / `session_*.go`: daemon hosting and account policy; `server_linux.go`: internal command dispatch |
| Wire protocol | `protocol.go`, `mux.go`: shell/application messages; `internal/transport/`: bounded frames, pipe ownership, smux tuning, shared OpenSSH ownership/session/subsystem arguments, forwarding-option filtering and SSH signal handling |
| Release version rules | Root `version.go`: exact matching and ordering of full releases; mount handshakes check equality locally |
| Direct copy | `cp_command.go`: batch preflight, interaction and independent SFTP transport; `cli_cp.go`: cp grammar |
| Approval | `internal/review/`: request/record models, operation validation, bounded storage, expiration and single-use claims; root `review*.go`: consent, connection policy and execution coordination |
| File transfer | `internal/files/cp*.go`: regular-file validation, local destination preflight, exact-size copies, verified uploads, no-clobber publication and restricted SFTP handlers |
| Local daemon | `local_daemon.go`: singleton startup, request serving, forward ownership and mount-worker supervision; `local_command.go`: shell creation and command dispatch; `local_terminal.go` / `local_attach.go`: shell lifetime/management and attachment; `local_shell_registry.go`: private registration records and listener ownership |
| Mux | `internal/tmux/`: exclusive ownership primitives, Linux PTY / Windows local ConPTY, input-mode tracking, native redraw and client console I/O |
| Terminal presentation | `internal/termui/`: status formatting, line editing, local path parsing/completion and platform console support; root `cli_ui.go`: input ownership, selection and CLI display adapters |
| Independent resources | `resource_runtime.go`, `resource_status.go`, `resources.go`: worker startup, private records, control and exit receipts |
| Forward | Root `cli_forward.go` / `forward*.go`: command mapping, authorization and worker integration; `internal/forward/`: transport-only forward configuration/validation, SSH mapping arguments, foreground/background process execution, reconnects, heartbeats and bounded diagnostics |
| Mount | `internal/files/mount*.go`: mount lifecycle, SFTP adapter/limits, caches, notifications, capacity, statistics and FUSE/WinFsp; root `mount*.go`: CLI, paths and worker integration |
| Private state | `internal/state/`: state-root directories, complete-write publication and platform privacy checks/ACLs; callers retain record schemas, name/version policy and lifecycle locks |
| Object names | `object_names*.go`: shared name validation, allocation, reservation and platform locks for shells, resources and reviews |
| Inventory | `inventory*.go`, `list*.go`: status views, lifetime policies, tables and stable numeric IDs; `pstree.go` / `pstree_state.go`: display-only ownership contexts, attachment sources and bounded snapshots; `peers*.go`: advisory peer records and synchronization |
| Distribution | `install.sh`, `install.ps1`, `scripts/package.py`, `scripts/release.sh`, `.github/workflows/`: installation, packaging, CI and releases |

Forward's application record retains names, server selection and approval shell/PID/ID;
`internal/forward.Config` contains only transport settings. The application supplies
an opaque remote helper command, streams, authentication environment and resource
callbacks; it owns source-shell checks and exact release policy. The engine owns
forward mapping, retry authentication restrictions and process execution. Shared
OpenSSH ownership and filtering live in `internal/transport` because copy, mount,
recent connections and peer synchronization also use them.

Application transport callers use `internal/transport` directly rather than
local aliases for pipes, mux defaults or full writes. Shell executable selection
returns only the path; startup argument policy stays in `internal/shell`.
Forward retry parsing and validation share one range rule in `internal/forward`;
the CLI preserves usage-error classification. A forward attempt captures one
authentication environment for both executable selection and execution. Its
heartbeat factory remains per-attempt: it runs after readiness and creates a new
peer identity on reconnect. The application encodes a complete record once;
`internal/forward` receives bytes and an error, owns only transmission, and joins
the heartbeat writer after cancellation/pipe closure before returning. Process
completion is published by closing a done channel; all return paths wait for it.
The resource stop watcher is also cancelled and joined before acknowledging stop,
so an acknowledgement cannot race ahead of watcher or transport teardown.

Copy/review CLI code delegates destination preflight to `internal/files` while
retaining prompts and consent. Preflight creates nothing; the transfer mechanism
still enforces no-clobber publication. Mount keeps versioned agent command/setup
selection in the application and delegates SSH session/subsystem arguments to
`internal/transport`. Shell account validation and resource coordination remain
in the root because they cross module lifecycle boundaries.

Business code calls `internal/state` directly through `Dir`, `Private`,
`WriteJSON` and `WriteFile`; the package depends only on shared byte writing,
not shell, review or resource models. It preserves the existing Unix ownership
checks and Windows DACL behavior. Approval exclusive creation and file-transfer
no-clobber publication remain with their owning modules; atomic state replacement
does not replace their policies. Record freshness, capability validation and
cross-record locking remain application responsibilities.

Files group complete responsibilities rather than individual helpers. Command
coordination stays in the root; terminal mechanisms stay in `internal/tmux`,
attachment I/O and startup policy in `internal/shell`, presentation primitives in
`internal/termui`, and framing in `internal/transport`. Within `internal/tmux`,
`console.go` groups portable console stream codecs and boundary cleanup;
`console_*` and `keyboard_*` keep platform adapters separate. Native terminal
resize/redraw belongs with each `terminal_*` implementation. Startup regressions
are grouped in `shell_startup_integration_linux_test.go` and
`internal/shell/startup*_test.go`. Mount tests group metadata consistency,
directory prefetch, and platform filesystem behavior rather than individual
callbacks. SFTP request bounds and timeout supervision share `sftp.go`;
platform mount capability/capacity callbacks live with their filesystem adapters.
`internal/review/review.go` owns the bounded request-record lifecycle;
`internal/termui/input.go` owns path input and completion, and `log.go` owns
presentation formatting. Platform and test build constraints remain separate
even when a file is small.

The root `main` package owns command routing, product policy and lifecycle
coordination. CLI dispatch and release/update policy remain in the root program. Product options are scanned in one place: valued options accept either
`--option VALUE` or `--option=VALUE`, duplicate product options and values on
boolean flags are rejected, and `--` begins the separately validated OpenSSH
suffix. Only shell launch/spawn/attach accept native forwarding flags. Each command
maps the scanner result to its own configuration; execution code and internal
packages do not interpret public option tokens. Internal helper protocols keep
explicit positional validation separate from the public CLI.
Local shell names use `--local NAME`; the redundant shell `--name` option and
hidden `shell` alias are removed in favor of `spawn`. Mounts and forwards retain
`--name`, which names their independently managed resource. Plain SFTP mounts
reject `--server`, since `--no-watch` does not run the remote program.

`session` remains the umbrella lifecycle term for managed shells, mounts and
forwards. List numbering and ID-based actions live together in `list_ids.go`;
they do not form a second task execution model. `worker` names an actual
background process, not an in-process shell listener. `job` is used only for the
Windows Job Object primitive. Shared remote status uses peer records. State-file
paths, stored JSON fields and internal wire verbs retain their existing formats.

Internal packages have narrower responsibilities and do not depend
on application types or mutable application state. The copy implementation in `files` knows nothing
about approval records, shell identities or the release version; callers authorize
operations before constructing its source/target views. Receiver handles and
publication state stay private. `termui` owns local status and line-editor interaction, including path completion
and prompt-console restoration. The program decides when an editor may consume
stdin and closes it before handing input to SSH. Raw attached-terminal input and
mouse modes belong to `tmux`; application code coordinates remote PTY streams.
The `review` package owns the stored request model and approval file lifecycle.
The program supplies platform privacy checks and validates saved connection policy;
name reservations and dispatch to shell, file or forwarding execution remain in
the program. Stored JSON and control-message field names remain unchanged.
The `forward` package runs the independent forwarding process and owns retry,
startup-marker and shutdown handling. The program constructs a fresh authenticated
command for each attempt and supplies readiness, status and public-heartbeat hooks.
The package does not know resource records, approval identities or daemon control
capabilities. Signals, name reservations and worker commit coordination stay in the
program; cancelling its context closes only that forwarding transport.
The same flat `files` package owns the mount filesystem engine and transient remote agent. Its configuration
receives an executable, already validated SSH arguments and the current release;
it never parses product options or installs software. The optional lifecycle
interface carries worker readiness, stop requests, transport environment,
operation timestamps and peer synchronization callbacks. Resource capabilities,
name reservations and worker HTTP endpoints remain in the application. Mount
notification messages define only mount fields; framing, message encoding and
smux configuration are shared with shell traffic through `transport`.
The package keeps `cp*` and `mount*` file groups beside shared SFTP framing,
limits and file validation. Copy and mount callers use these primitives directly;
neither command depends on the other command adapter. `MountConfig` and
`MountLifecycle` describe mount-only orchestration. Stop requests carry reply
channels, so the resource manager does not depend on mount-owned control types.
Package-level tests live beside their implementation; workflow tests stay with
the application.

Remaining filename groups are coupled through `message`, `operation`, options,
setup state and platform hooks. Protocol messages embed setup, approval and peer
task types, so moving them requires deciding type ownership first. Shell process
identity validation and update locking must move with their lifecycle callers.
Keep CLI routing and assembly in `main`, move cohesive units with their tests and
both platform implementations, and avoid exporting mutable state merely to make
a move compile. The release linker still sets `main.version`.

Business source files follow command names: `cp`, `mount`, `forward` and
`review`. Historical internal function and wire names remain where changing them
would add unrelated compatibility work.
Source comments explain local invariants and algorithms; cross-component design
belongs here, not in file headers or a second documentation tree.

## Terminal and mux design scope

The mux design manages one terminal per shell session, with creation, listing,
exclusive attachment, explicit detachment, resize and termination. Multiple
independent sessions are allowed; split panes, pane layouts and tiling are outside
the project scope. Linux uses native PTYs. Windows uses native ConPTY for an
ordinary local mux running local programs, without requiring SSH or a remote
host. Windows continues to support its existing role as an SSH client to Linux;
it will not provide a remote shell server.

Local shell workers use a private record and a token-authenticated loopback
connection on both platforms. Startup transfers ownership only after private
publication and acknowledgement. Shells retain the creating directory and strip
inherited remote helper variables. Exact release matching applies locally too;
local attachment never updates or replaces another worker. Explicit detach
preserves the terminal; transport loss releases the attachment. Windows never
provides SSH remote shell hosting.

Numeric task IDs are persisted under the local state directory with an OS lock.
They increase without reuse and bind immutable instances rather than names.
Local shell and resource capability hashes identify workers without exporting
capabilities; remote shells expose a random instance value. Name-based commands
remain supported. Cached remote listings do not prove liveness; ID operations
revalidate the instance at the authority performing the operation.

The flat `internal/tmux` package combines client console adapters (`console*`,
`keyboard*`) with managed terminals (`terminal*`), attachment lifecycle and
shortcut parsing. It does not depend on the external tmux program. Commands,
SSH, approvals, release policy and spawn ownership stay in the application.
The shared infrastructure provides native Linux PTY and
Windows ConPTY startup, byte I/O, resize, process wait and terminal close. Linux
shell startup, attachment ownership and terminal resize/close use this layer.
The package owns terminal and attachment mechanisms, while the application owns
SSH, exact release checks, names, approvals, account process validation and exit
receipts. Preparing an attachment must succeed before its identity becomes the
owner. A stale attachment cannot detach or terminate a newer owner; explicit
detach preserves the terminal. Client loss stops its writer and releases attachment ownership while preserving the daemon-owned terminal. A failed initial attachment rolls back
its newly created terminal; failed reattachment preserves an existing detached
terminal. Linux duplicates and rewraps the PTY master as a pollable nonblocking
file after startup, and all later ioctls use SyscallConn instead of Fd so resize
and redraw cannot silently disable cancellation. Linux reaps the shell independently
of PTY output and ends output draining one second after shell exit, so a descendant
retaining the slave cannot keep a finished shell session alive.
`mux.go` adapts application handshake metadata to `internal/shell`,
which owns serialized attachment writes, input barriers, bounded exit draining
and detach confirmation for local and remote shells. `internal/tmux` owns
exclusive ownership primitives and platform input cancellation. Local record and
worker management stays separate from Linux remote account/setup policy.
The root command adapters translate application metadata without exposing it to
the package. Windows ConPTY is the foundation for local mux
sessions; Windows remote shell hosting is outside the project scope. The remote
daemon, authenticated helper endpoints and remote account process management
remain Linux-only.
Windows local sessions prefer `SHELL`, then `COMSPEC`, falling back
to `cmd.exe`; installed PowerShell versions are not selected automatically.
The Windows local daemon starts without a console; each shell allocates its own
ConPTY. Local reattachment requests a native redraw after restoring output
ownership, without injecting a keystroke into the shell.
Terminal bytes pass through, targeting common VT sequences used with
Alacritty; this layer does not emulate a screen or promise screen restoration.
At the Windows client output boundary, a bounded streaming filter removes only
DEC private mode 9001 (Win32 input) set/reset requests, including combined mode
lists. OSC/DCS payloads and other modes remain opaque. Forwarding a nested
ConPTY's request would make the outer console encode Alacritty's VT arrow bytes
as literal Win32 character events, so the inner shell sees `[A` instead of Up.
On entry the client clears any inherited mode 9001 and keeps `ReadConsole` in VT
mode; native upstream key events are still translated to VT by the outer console.
Windows regression tests exercise both upstream formats and cursor-position
replies in an isolated native ConPTY.
Callers must continuously drain output while a session is running, wait for the
process and close the terminal when its lifetime ends. Wait is idempotent and
concurrent callers receive the same result. Linux Close interrupts blocked I/O. Windows uses non-inheritable pipes
with a requested 64 KiB buffer and an explicit absolute executable path at process
creation. Command-line, directory and environment conversion happens before
allocating ConPTY. Repeated Windows resize requests for the current size are
skipped; failed resizes do not update the remembered dimensions.
ConPTY child startup explicitly excludes the worker's redirected standard
handles, so closed startup pipes cannot become the shell's input or output.
Each ConPTY process starts suspended and joins a kill-on-close job before it
runs. Closing the terminal job ends its process tree even if console shutdown
notification races with startup. Detach leaves that job open.
Close discards unread output before closing ConPTY, avoiding shutdown dependence
on an active reader on older Windows versions. Close does not guarantee
termination of escaped descendants.

The PTY lifecycle review uses [tmux spawn.c](https://github.com/tmux/tmux/blob/master/spawn.c)
and [window.c](https://github.com/tmux/tmux/blob/master/window.c) for process and
output ownership, and [psmux ConPTY](https://github.com/psmux/psmux/blob/master/crates/portable-pty-psmux/src/win/conpty.rs)
for pipe buffering. Shutdown ordering follows the
[ClosePseudoConsole contract](https://learn.microsoft.com/en-us/windows/console/closepseudoconsole).
These are implementation references, not runtime dependencies. Linux PTY EIO
after slave closure is exposed as EOF by the shared terminal reader. Raw VT
output cannot use psmux-style replacement of queued screen snapshots: dropping
arbitrary bytes loses terminal state. The current remote output path retains
ordered bytes and bounded stream backpressure with a five-second write deadline;
control traffic uses a separate stream. A future screen-state implementation
would be required before stale output can be replaced with a fresh snapshot.

## Mux controls and implementation boundaries

Local input uses a Ctrl+B prefix, following the basic tmux/psmux convention.
The supported bindings are defined by command help. A pending prefix survives
input-read boundaries without a timeout; a doubled prefix sends one literal
Ctrl+B. Unknown combinations are forwarded unchanged. Escape sequences are
forwarded immediately, including a lone Escape. Bracketed-paste delimiters are
recognized across reads, and their contents bypass shortcuts.
Unbracketed paste is indistinguishable from typing and follows normal shortcuts.
`--no-escape` and recovery mode forward all bytes without local interception.
Session commands query the current daemon before detaching. Cycling uses lexical
name order with wraparound and includes only the current shell and detached
shells, skipping other clients' attachments. With no other shell, cycling stays
put. The chooser accepts a displayed number and Enter; Escape/Ctrl+C cancels.
New-session preflight checks automatic-name availability without reserving it.
All target attachment/creation checks run again on reconnect; a race never grants
a takeover. If target setup fails before interactive readiness, the client tries
to reattach the original session once. A concurrently occupied or ended original
session can still prevent recovery. The last-session key tracks the previous
successful switch within this client run.

Switching returns through an iterative client loop, releasing the old transport,
raw terminal state, attachment tracking and joined keyboard reader before the next
SSH launch. Linux polls input with a bounded wait; Windows cancels the pinned
ReadConsole thread and joins cancellation before releasing that thread. No old
reader may compete with subsequent SSH authentication or terminal input. Switching
uses a new SSH transport and may require authentication again. Bytes trailing a
session command in the same input read are discarded, avoiding accidental input
to a different session; input during target lookup is also discarded.

New Linux launch/spawn shells use login and interactive startup (`-l -i`) for
bash, zsh, fish, sh, dash, ash and ksh. Unknown executable names require
`--no-login`, which retains ordinary `-i` startup. Windows keeps normal startup
and rejects `--no-login`. PowerShell support targets Windows; Windows shells are
local-only.

`--no-profile` selects non-login interactive startup on Linux: Bash receives
`--noprofile --norc -i`, Zsh `-f -i`, Fish `--no-config -i`, and sh `-i` with
`ENV` removed from the child environment. Windows PowerShell receives `-NoProfile`
and CMD `/D`; no banner-suppression option is added. Other shell names reject
`--no-profile`. The supported sh contract is non-login startup plus disabling its
ENV hook; implementations may differ. Zsh's system zshenv still runs, and inherited
variables (including PATH and application identity) remain. This is startup-file
suppression, not an isolated environment or a security boundary.

Creation-only options travel in the initial attachment request and are rejected
on an existing shell. Switching to an existing shell never reruns startup files;
Ctrl+B c inherits the current client's startup preference. Recovery retains its
ordinary interactive startup. Shells own profile selection and failures; sshore
neither sources profiles nor retries failed startup as a different mode.

This initial input path handles conventional VT control bytes; enhanced keyboard
protocol encodings are passed through rather than interpreted as shortcuts.

The input writer preserves data/command order. Before shortcut detach it sends
an input barrier on the terminal stream and waits up to five seconds for the
daemon to acknowledge writing preceding input to the PTY. The UI then sends
detach on the independent control stream. If delivery remains blocked for five
seconds, the client reports unconfirmed input and still requests explicit detach;
blocked input must not turn a preservation request into transport-loss teardown.
The daemon cancels and joins the former attachment's input lease before allowing
a successor. Bytes already accepted by the PTY remain application input; remaining
bytes from the canceled write are discarded. A successful barrier is not proof
that an application has executed or saved the input. The writer pauses for local command
handling so subsequent input cannot overtake a command; queues remain bounded.
Redraw uses the existing foreground redraw request, without injecting Ctrl+L or
Enter. Help and connection information print concise local lines; they do not
provide an overlay or restore the application's screen.

| Boundary | tmux / psmux reference | sshore decision |
| --- | --- | --- |
| Prefix keys | [tmux defaults](https://github.com/tmux/tmux/blob/master/key-bindings.c), [psmux keys](https://github.com/psmux/psmux/blob/master/docs/keybindings.md) | Small fixed Ctrl+B table for independent sessions; no binding language or pane commands. |
| Screen recovery | Terminal state and rendered screen snapshots | Raw VT transport with bounded input-mode observation; redraw remains best effort. |
| Slow output | Screen-aware redraw / stale-frame replacement | Ordered byte backpressure; arbitrary byte dropping would corrupt terminal state. |
| Attach ownership | Multiple-client mux models | One exclusive attachment per shell; stale clients cannot release its successor. |
| Lifetime | Persistent mux server / session model | Detach and detected transport loss preserve the shell; exit/cancel ends it. |

## Connection and shell lifecycle

`launch` reserves an unused name and creates a PTY shell. Automatic names come
from a fixed pool; callers can supply a name when the pool is exhausted. `attach`
resolves an existing detached shell and never evicts another client. Local
`connections/recent.json` retains up to 32 hosts, ordered by successful interactive
attachment, with the latest shell name, remote executable and SSH transport
options and original SSH working directory for each host. Relative SSH key/config
paths retain their meaning without changing local download paths. It captures no
typed authentication answers and excludes
native forwarding flags. Atomic private writes keep metadata complete; concurrent
clients are last-writer-wins. With no host, attach reuses the newest record; with
a host, its record supplies an omitted name and transport defaults. Explicit
server/SSH options override saved defaults. Stale names fail normally without
creating a replacement or taking over an attached client. Without a saved record,
omitting the name selects the only detached shell or opens the picker. Selection
uses exact identities or displayed numbers, supports `/text` filtering, and does
not hold the account setup lock while waiting for terminal input.

Public names are labels backed by private metadata and lifetime locks. Rename
changes the label while preserving the daemon's socket/helper identity. A
successful local rename updates the recent attach target and cached labels while
retaining the original confirmation time; it does not claim a new liveness check. Pending
approvals keep their original name and shell PID and must be requested again
after a rename. Process-tree listings are best-effort snapshots, not identities
that can be used directly for signaling.

Detach and detected transport loss preserve the shell. Exit or cancellation
closes its PTY. This follows terminal hangup semantics; it is
not a general process containment mechanism. Detached output and terminal screens
are not replayed. Reattachment requests foreground redraw. When the shell itself
owns the PTY foreground group, a one-column resize and 30 ms restoration prompts
readline to redisplay even at unchanged terminal size; no input is injected and
unsubmitted commands remain unsubmitted. Other foreground applications receive
SIGWINCH. Restoration checks the current size to avoid overwriting another resize.
The bounded input-mode observer restores mouse reporting, focus reporting (1004)
bracketed paste (2004), color-scheme (2031), visibility (2033), in-band size
reports (2048), and application cursor keys (DECCKM, mode 1), including
private-mode saved values, after detach or
muted output. It observes control sequences across read boundaries and ignores
OSC/DCS payloads. In ground state it scans directly to ESC, skipping ordinary
text without per-byte mode dispatch; partial sequences keep their parser state
across reads. Traditional application keypad mode (`ESC =` / `ESC >`) is also
tracked; connection cleanup returns cursor keys and keypad to normal mode.
These basic key modes follow `DECCKM`, `DECKPAM` and `DECKPNM` in
[tmux input.c](https://github.com/tmux/tmux/blob/master/input.c).
Enhanced keyboard protocol stacks, screen state and scrollback are not
reconstructed. This follows the separation of input-mode state from screen state
in [tmux input.c](https://github.com/tmux/tmux/blob/master/input.c).
The supported interactive editors are GNU readline in bash, ZLE in zsh,
fish's editor, PSReadLine in Windows pwsh, and the Windows console editor in cmd.
The persistent shell/console owns the edit buffer, cursor, history and undo state;
sshore does not serialize or reconstruct these editor internals. Redraw does not
inject Ctrl+L, Enter or editor-specific commands. Linux PTY regressions exercise
unsubmitted input, insertion at a retained cursor position, and prompt redisplay
in bash, zsh and fish. These checks do not establish complete multiline, vi-mode,
completion-menu or custom prompt/plugin rendering compatibility.

Linux job control remains owned by the controlling PTY and shell: Ctrl+Z, bg,
fg and Ctrl+C are covered across detach/reattach for bash, zsh and fish. Tests
wait for the job's actual foreground process group before sending interrupts.
Windows local mux retains the same ConPTY and shell process on detach; pwsh
with PSReadLine and cmd use native console editing and interrupt semantics.
Unix suspend/fg/bg behavior is not added to Windows, and PowerShell jobs are
managed by PowerShell. ConPTY redraw temporarily changes width; cached dimensions
track a failed restoration so a later resize can retry. Windows coverage for this
review is static inspection and amd64 compilation, not interactive execution.
Native validation must still cover pending input and cursor retention, prompt
redisplay and Ctrl+C with PSReadLine loaded and with cmd. Windows remote shell
hosting remains outside scope.

Terminal mode restoration and application redraw remain best effort. See [termination guarantees](SECURITY.md#files-and-forwarding).

Attachment also preserves an open control-string introducer and pending ST escape
without buffering its payload, so the remaining opaque bytes cannot become new
input-mode commands. This preserves parser context, not the original OSC/DCS
operation or its discarded contents. Windows' nested Win32-mode filter recognizes
embedded C0 controls and DEL in CSI parameters, retaining those controls while
removing mode 9001.

Focused parser regressions use every attachment cut point and bytewise output.
The reference
cases are Zellij's fragmented OSC/CSI and embedded-paste tests in
[stdin_ansi_parser_tests.rs](https://github.com/zellij-org/zellij/blob/main/zellij-client/src/stdin_ansi_parser_tests.rs)
and psmux's
[missing paste terminator test](https://github.com/psmux/psmux/blob/master/tests/test_vt_paste_missing_close.ps1).
Here paste bytes are streamed with bounded delimiter state; a missing terminator
keeps shortcuts disabled until the closing marker rather than applying a timeout
which could execute pasted Ctrl+B commands.
The same completed-marker boundary is covered by
[tmux's paste regression](https://github.com/tmux/tmux/blob/master/regress/tty-keys-paste.sh):
a preceding Escape stays literal, paste contents bypass bindings, and the first
key after the completed end marker uses bindings again.

At connection boundaries, console cleanup cancels incomplete control sequences,
ends synchronized output (2026), closes OSC 8 hyperlinks, resets text style and
shows the cursor before returning to local prompts, in addition to disabling
mouse, focus, paste, color-scheme, visibility and in-band size reporting.
The additional report modes follow [Ghostty modes](https://github.com/ghostty-org/ghostty/blob/542611b3db75d320907ed09fac9a61b572ca86d9/src/terminal/modes.zig);
these subscriptions must not outlive the attached application. This follows the explicit prompt teardown in
[Zellij's client](https://github.com/zellij-org/zellij/blob/main/zellij-client/src/lib.rs)
and [psmux's scrollback client](https://github.com/psmux/psmux/blob/master/src/scrollback.rs).
Unlike their screen renderers, this byte transport does not generate synchronized
frames or restore alternate screens it does not own. Unsupported cleanup modes
remain terminal-dependent; redirected output and Unix `TERM=dumb` receive none.

The full release version is the compatibility identifier at setup and handshake.
There is no separate sshore protocol counter. Running daemons are replaced through
account setup, not upgraded in place. Recovery uses a transient PTY on the existing
SSH connection; see [setup and recovery safeguards](SECURITY.md#remote-setup-and-updates).

Remote bootstrap first uses the exact installed release without networking. A
missing or older program stages a matching coordinator via the remote installer,
with up to two retries for transient HTTP failures. Failed remote preparation
sends a bounded `setup_download` frame for Linux amd64 on the existing SSH pipes.
The client uses its fixed release URL and exact version, verifies SHA256SUMS,
extracts only the regular `sshore` archive entry, and caches the archive/manifest
under the user's cache directory in `sshore/releases/VERSION`. Cached artifacts
are verified on every reuse; unavailable/pruned releases never substitute another
version. Downloads, archives, executable size, and redirects are bounded.

The client sends an ASCII size/SHA256 header and exactly that many executable
bytes. The shell consumes exactly the header and payload, verifies length/hash
and the executable's full version, then starts the coordinator on the same
authenticated connection. The remote spawn owner relays this bounded transfer
without releasing its SSH connection. The target installation is untouched until
the existing account-lock and update-confirmation checks succeed. Preparation
failures return framed errors and remove temporary staging.

Default `mount` uses this same setup before its mount-agent handshake, without
creating a persistent shell. Its optional transport preparation callback completes
before SFTP/mount setup and shares the startup cancellation/timeout. Background
mounts relay update choices and setup status through the launcher's authenticated
prompt broker; no worker reads terminal input. Enter cancels helper setup; manual
recovery remains an explicit shell connection, never a hidden mount recovery PTY.
`mount --no-watch`, direct `cp`, ordinary forwarding, inventory and cancellation
do not enter automatic installation. WinFsp/FUSE prerequisites remain local setup.

## Streams and approved operations

smux v2 carries logical control, terminal and file streams over one SSH connection;
these are not native SSH channels. The receive budget is bounded across streams,
with per-stream windows. TCP head-of-line blocking remains possible. Keepalives
detect transport loss and release attachment ownership. Numeric framing
limits and buffer sizes are defined in `protocol.go` and `mux.go`.

A remote helper submits bounded operation metadata. The connected client saves a
local pending record and detaches. A separate local `review` invocation selects
the record, collects consent, consumes it, reconnects and checks the original
shell before executing. Setup updates or recovery skip approved operations.
The authorization rules belong in [Local approval](SECURITY.md#local-approval).

File batches run sequentially and stop at the first failure. Both direct and
approved copies use the same SFTP upload/download implementation in `internal/files`.
Uploads create private temporary files, verify stored bytes by SHA-256 readback,
and publish with a no-overwrite hard link. Downloads check the exact source size
and sync a local temporary file before publication. Authenticated SSH provides
transport integrity; neither mechanism authenticates file provenance.

Direct cp opens a system OpenSSH SFTP subsystem without installing sshore,
creating a shell or mounting a filesystem. It preflights the whole batch. A bare
`-` selects the invoking process's stdin/stdout for one direct remote copy.
Stdin is streamed into the remote temporary file while counting and hashing
bytes; EOF determines the size used for readback verification and publication.
It is not spooled locally. Stdout writes directly and may contain partial data
on failure. Streams cannot participate in remote approval requests because
those detach and resume in another process.
Approved copies instead open a client-initiated smux stream on the revalidated
shell. A bounded metadata handshake selects one file, then switches to SFTP.
The embedded request handler exposes only virtual `/source` or `/target` and
one upload temporary file; it cannot list directories or access other paths.
Before linking an approved upload, the server checks its size and syncs it.
Stream termination closes handles and removes its temporary file.

Progress reports batch position, smoothed speed and ETA. Completed files remain
when later operations fail. Cancellation closes only the transfer stream (or
the direct copy's SSH pipes); cleanup is best effort after process termination.
The client releases terminal input before SSH authentication and retains the
existing resume/`--stay` behavior after approved copies.

## Inventory and shell history

`list` uses one ID-ordered task table with a host column. It combines local
task checks with cached remote shell records and marks remote hosts as unqueried.
The connected client holds a local lifetime lock; this confirms the local
connection process, not continuous remote health. A ready handshake records
attached state. A detached acknowledgement records detached state before return;
loss without acknowledgement marks remote state unconfirmed. Records show their
last confirmation time, and older recent-connection defaults have unknown state.

`list HOST` announces the target before SSH authentication and requests only that
account's inventory. It never mixes in the client's local resource table or runs
setup. `--verbose` adds process trees and detailed resource diagnostics. Visible
same-user daemons across runtime directories are queried; unreachable shells make
the result incomplete. Other managers' resources are marked read-only in the same table. Successful queries refresh shell history; incomplete or failed queries
never establish that an unreported shell ended. A complete query records absent
shells as not listed, without claiming how they ended.

Connection startup reads inventory through the existing authenticated mux control
stream, one bounded row per frame. It excludes the current shell and merges local
resources connected to the host; the renderer and stable IDs are shared with
`list`. Incomplete results are labelled rather than represented as live certainty.

`cancel [HOST] --all` collects a fresh snapshot and requires the literal `cancel`
after displaying its targets. Without HOST it excludes remote history; with HOST
it includes the remote account's managed shells/resources and locally managed
resources connected to that destination. Other managers' peer advertisements are
read-only. Each cancellation revalidates the snapshot's shell instance or resource
capability hash; newly created or same-name replacement tasks are excluded. Partial
failures are reported while independent targets continue; busy mounts can remain.

Linux remote shells share a daemon per private runtime directory. The SSH bridge
requests a shell through a private Unix control socket; the daemon owns its PTY
from creation and preserves the launching connection's environment and directory.
Per-shell sockets and locks retain helper, attachment and rename identities.
The remote daemon exits after its last shell and outstanding handlers finish.
Legacy per-shell daemons remain discoverable and manageable during transition.

Process discovery expands a shared daemon into its live shell endpoints, checking
Unix socket peer ownership. Update snapshots therefore still show every affected
shell. Explicit cancellation sends the confirmed instance to its owner and stops
only that shell's identity-checked process tree; cancelling one shell cannot stop
its siblings. Confirmed version updates may terminate the shared daemon and all
of its identified shell tasks together, under the existing account setup lock.

## Temporary shells (`spawn`)

`spawn` creates the same single-terminal shell as `launch`, but records the
launching process's parent PID and creation identity. The local daemon checks
that owner every 500 ms; exit, zombie state or PID reuse ends the spawn. This
binds to the actual parent process, including a wrapper if one launches the CLI;
reattachment never transfers the original ownership. Local Linux cleanup uses
the identity-checked task snapshot/signaling path. Windows starts a spawn's
ConPTY process suspended, assigns a kill-on-close job, then resumes it; closing
the terminal job ends its descendants. Persistent `launch` behavior is unchanged.

For a remote spawn the local daemon owns one lifetime SSH connection. The
launching client handles SSH askpass and setup choices before entering raw mode.
The remote bridge first claims an exclusive lifetime connection to the exact
new temporary shell endpoint, then serves a bounded outer smux transport. The
initial attachment uses one inner stream; detaching closes only that stream.
The outer smux heartbeat continues in the background (10-second interval,
60-second receive timeout); it is independent of subsequent attachments.
Later attachments use the usual SSH route. The original connection remains
until owner exit, remote shell exit or transport loss. Closing its lease invokes
normal instance-checked shell cancellation. Keepalive detection bounds silent
network-loss detection; there is no automatic reconnect or lifetime transfer.
Recovery stays a foreground-only transient shell on the authenticated connection.

Temporary status travels in live handshake/inventory metadata and cached shell
snapshots. The existing task column renders `spawn/NAME` versus `launch/NAME`;
IDs, names, attachment states, rename and cancellation keep their existing rules.
Ctrl+B c from a spawn creates another spawn under the switching client's parent.

## Shared local daemon and resource workers

One daemon per current user and private state root is started on demand. A
startup file lock serializes launchers; a lifetime lock prevents replacement of
a live, unresponsive daemon. Clients require an exact full release match, with
no automatic replacement or task termination on mismatch. Separate explicit
state roots remain isolated. Existing independent workers are not migrated or
terminated; their private records remain usable.

The daemon owns all newly created local shell terminals from startup, before the
first attachment. Each shell keeps its own authenticated loopback endpoint,
input writer, output pump and lifecycle lock. A daemon bookkeeping mutex only
counts active requests/tasks; it is never held during terminal I/O, authentication
or resource startup. The existing private ID index stays stable across daemon
restarts. Local client loss releases attachment ownership after stopping its
writer. Explicit shell exit/cancel ends the terminal. A daemon crash can end all
its local shells; terminal state cannot be reconstructed from metadata.

The daemon directly owns forward SSH subprocesses; no extra sshore process wraps
each forward. Mounts retain independent workers because native filesystem work
needs a separate failure boundary. The daemon proxies mount startup/commit and
reaps workers; filesystem payloads bypass the daemon. Mount workers can survive
daemon failure and remain accessible through their private control records. The
request carries the launching client's working directory and environment rather
than inheriting whichever shell first started the daemon. Environment values are
not persisted in daemon metadata. Local shells select the executable from the
launching client's `SHELL` (a path, not a command with arguments). Linux falls back
to `/bin/sh`; Windows falls back to `COMSPEC`, then `cmd.exe`, and adds no Unix
shell flags. Reattachment preserves the existing shell. Windows mount workers
and directly owned forward SSH processes retain a hidden console for OpenSSH/askpass inheritance, separate from the console-free daemon.

The launching CLI handles authentication through a temporary askpass broker
until startup acknowledgement. Losing it before commit abandons startup. Once
the task acknowledges commit, the daemon retains it even if the launcher
misses the acknowledgement. After all owned shells/workers and startup requests
end, the daemon exits after 30 seconds idle. Its private diagnostics are written
to `daemon/local.log` under the state root. Process IDs in metadata are diagnostic
only, never authority to signal a process.

 Shells, mounts, forwards and pending reviews share one ordered
154-name Pokemon pool and one local account namespace in the shared state root.
Explicit names and shell renames use the same conflict checks. Separate computers
and deliberately isolated state roots do not coordinate names. New allocations
also check legacy resource lifetime locks and visible same-user shell labels
across runtime directories; existing objects are not renamed.
Per-name startup reservations bridge creator-to-worker/daemon handoff; lifetime
locks keep active names occupied. Pending records reserve their names until
consumption, cancellation or expiry. Approval takes a reservation before consuming
the record, and an approved forward inherits that name without a reuse gap.
File requests retain their name through execution. Exhaustion requires an explicit
name for shells/resources; new reviews fail without executing anything.
Authenticated loopback control performs status and cancellation without signaling
a PID read from disk.
Resource records, peer snapshots, shell names and exit receipts share private
temporary-file writes: sync and close before replacing metadata. File transfers
retain no-overwrite publication; approvals retain exclusive creation and consumption.
Consumption exclusively creates a claim file, renames the pending record to a
non-pending name, then removes both. Direct deletion is insufficient on Windows,
where concurrent delete requests can both succeed while a file is delete-pending.
A crash after claiming prevents replay; a leftover claim requires manual cleanup.

The public transfer commands are `cp SOURCE... TARGET` and
`forward LISTENER SERVICE`. `origin:` and `:` denote the client's endpoint;
`origin:PATH` and `origin:PORT` can fill it explicitly. Local direct commands use
`HOST:PATH` or `HOST:PORT` for the remote side. Remote helpers use the current
server context and never initiate another SSH connection.

Forward requests always store the listener in `port` and service in `target_port`.
Reverse means the server listens. The client port stays zero until review, even
when `local_hint` suggests a port; a suggestion never authorizes execution.
Review fills that missing port and clears the hint.
Unresolved requests cannot execute. File requests likewise retain an optional
local path hint; review validates the selected path and clears the hint before
execution. Relative local suggestions are resolved in the review process's
working directory, with home expansion supported. Internal upload/download
operation kinds and forward/NAME resource IDs remain; the public upload/download
and publish/subscribe commands are removed.

Submitting all validated local paths or a valid local port grants consent;
there is no separate yes prompt. Enter accepts a displayed suggestion.
An empty port without a suggestion retries. File batches collect every path
before any transfer starts and reject duplicate destinations. Cancellation
during collection leaves the request pending.
Approved forwarding starts its worker immediately after local review and uses
one SSH authentication. The remote `forward-watch` command checks exact release,
source shell name/PID and detached state through a bounded local daemon RPC,
without reattaching or starting a shell. Native OpenSSH establishes the explicitly
approved listener before running this command; readiness is reported only after
source validation, and failure closes the transport. There is no separate setup
connection or software update in this path. Source shell lifetime remains
independent. Shutdown first closes stdin to let the watcher remove its lease,
then forces SSH termination after two seconds if necessary. Current-attempt SSH
stderr is bounded and returned to the launcher on startup failure, including
occupied/denied remote listener errors; full diagnostics remain in the worker log.
Peer inventory write failures warn once and retry without closing a live forward. Local forwards may opt into at most ten retries after an
established connection fails. Retry backoff is bounded; authentication is
noninteractive, host keys must already be trusted, and TCP connections are not
resumed. Cancellation also interrupts backoff. Initial startup failures are not
retried. Native SSH forwarding remains owned by its SSH connection.

Mounts own one system OpenSSH connection. By default it runs the transient Linux
amd64 `mount-agent` entry of the installed remote sshore, found through PATH or
`~/.local/bin/sshore` (or the explicit server option). A bounded handshake checks
both processes' exact full release before starting file service. This command does
not install/update software or inspect/terminate persistent shells. Missing or
mismatched installations fail with diagnostics; normal shell setup remains the
update path. The no-watch option instead opens the ordinary SFTP subsystem and
does not require remote sshore.

The agent uses the existing smux framing/configuration on this mount-owned SSH
transport: one stream carries SFTP and another carries watch subscriptions and
invalidation messages. There is only one SSH authentication, no second connection
or password cache. It is independent of any shell's mux. SFTP is proxied to the
installed OpenSSH `sftp-server` executable, discovered in standard Linux locations
or PATH, preserving its extensions. Servers configured only with internal-sftp
need an external executable for this mode, or can use no-watch mode. The child
and watcher end with the transport; a notification stream failure leaves SFTP live.

Remote fsnotify uses Linux inotify without cgo. Cached stat/lstat reads enqueue a
subscription to the parent directory; directory reads enqueue a subscription to
that directory. Only accessed
directories are watched, never a recursive scan: the client evicts LRU watches at
128 directories and the server independently enforces the same limit. Successful
subscriptions are renewed on access after 30 seconds; failures are retried after
one second. One background worker processes at most 128 queued paths plus one
active subscription;
repeated pending paths coalesce, and queue overflow falls back to TTLs. Filesystem
reads do not wait for subscription acknowledgement. First acknowledgement or recovery from a failed/reset subscription invalidates
affected cached results and in-flight fetches from the unobserved interval.
Successful renewal of a continuously confirmed watch preserves cached results
without extending their TTL. A reset racing with acknowledgement forces refresh
on subsequent access. Each
subscription exchange has a three-second deadline; timeout closes
only the notification stream and warns once before falling back to TTLs.

A dedicated reader coalesces filesystem events into a bounded set of at most
128 dirty directory paths even under network backpressure. The first invalidation
is sent immediately and bursts are coalesced with a 50 ms cooldown, including a
trailing invalidation. Ordinary changes carry the directory path and retire its
listing, child attributes, associated handle metadata and matching in-flight
responses. Unrelated cached directories remain usable. Removals, renames, watcher
errors/overflow and a full dirty set trigger a whole-cache reset and force
subscription refresh on subsequent access. An unscoped dirty message also falls
back to whole-cache invalidation. Notification loss clears the cache. No event is interpreted as a file
operation or causes write replay. The existing short TTLs remain unchanged:
unsupported filesystems, alias/hard-link changes and symlink retargeting can evade
notifications. This invalidates the shared backend cache, not OS page/attribute
caches or existing directory-handle snapshots; native Explorer change notifications
are not implemented. Statistics count remote-change invalidations separately from subscription
confirmations that retire potentially stale metadata.
Capacity queries use `statvfs@openssh.com` version 2: Linux exposes remote block
and inode counts, while Windows reports total bytes and bytes available to the
remote non-root user. Unsupported or unknown extension versions return an
unsupported-operation error instead of synthetic capacity; query failures remain
errors. Counts describe the backing filesystem, not a quota for the mounted
subdirectory. Replies are checked before integer narrowing and byte conversion.

Linux uses FUSE; Windows uses WinFsp. Linux mtime-only updates preserve the
SFTP-reported access time. Linux chmod and truncation use an available open file
handle, preserving its identity after path replacement. Timestamp updates and
Windows chmod remain path-based and are not atomic with remote path replacement,
even when an application holds an open file handle. Writable mounts require atomic-rename and
fsync extensions. There is no persistent write-back cache, conflict resolution
or replay. Linux read caching lasts until close/reopen unless direct I/O is used;
Windows rejects unrepresentable names instead of renaming them. Special files and
file locking are unsupported.

Windows handle metadata queries also use this bounded cache with a two-second
TTL. Entries are keyed by the open handle, not its pathname, and removed on close;
local mutations and remote notifications invalidate them. Repeated editor polls
therefore reuse metadata without extending its expiry or confusing a replaced
pathname with an already open file. Directory handles reuse path attributes across WinFsp enumeration reopens,
within the same TTL and invalidation rules. File contents remain uncached here.

Mount smux streams use a 1 MiB receive window instead of the shell transport's
256 KiB window, retaining the shared receive-memory cap. SFTP keeps its default
32 KiB packets and 64 concurrent requests per file for server compatibility.
The larger window and four prefetch workers trade memory and background requests
for less waiting on higher-latency connections. Longer metadata TTLs can delay
external changes when notifications are unavailable or miss an event; local
mutations and received notifications still invalidate caches immediately.

Both adapters use a per-mount LRU cache: path metadata expires two seconds after
the lookup starts, and directory listings expire after five seconds. Hits do not
extend expiry. The cache retains at most 4,096 metadata results, 128 directory
lists, 32,768 directory entries and 32 MiB of estimated retained storage (including
SFTP extended attributes). Oversized listings remain readable within the existing
wire budget but are not retained in the shared directory cache. Open handles keep
their own cursor over an immutable snapshot; those live snapshots are outside the
shared cache budget. Reopening may reuse an unexpired listing.

After a successful foreground listing, up to four background workers prefetch
at most 16 directly listed child directories, selected in name order. Hidden
names, symlinks and names that cannot be represented on Windows are skipped.
The newest listing replaces queued work and cancels the previous batch; repeated
listings with the same candidates within the directory TTL do not restart it.
Prefetch does not recursively schedule more reads. Each speculative listing has
a two-second context deadline and shares the normal bounded cache, TTL and
in-flight fetches. Subscription acknowledgement retires only affected directory
metadata and matching in-flight results, preserving prefetched siblings; handle
metadata is retired only for that directory. Ordinary remote changes are scoped to their directory;
uncertain or lost event detail still resets the whole cache. Workers register each directory before fetching it without
waiting for all other candidates. A foreground reader joining a cancelled or
timed-out speculative fetch retries with its normal request deadline. Shutdown
cancels work and closes the transport before joining workers. Prefetch reads
metadata only and can cause directory access on the remote server. Statistics
report `PrefetchStored`, `PrefetchHits` (first foreground use) and
`PrefetchDiscarded` (evicted, expired or invalidated before use). These distinguish
useful prefetch from speculative work that did not benefit a foreground read.

Concurrent misses for the same operation/path share a request within the same
mutation generation. Errors are shared only with current waiters, never cached.
Fresh directory reads seed a bounded number of Windows-compatible path attributes;
symlink entries are excluded because stat follows the target. Cached directory
results do not reseed attributes or extend their lifetime. Linux lstat entries
use a separate key space. Windows reuses unexpired path attributes for the preliminary open classification;
create/truncate classification stays live. Each regular-file open still obtains a
new SFTP handle, and its first handle stat queries that handle rather than reusing
pathname attributes. Subsequent Windows handle queries use the short TTL above.
Local create, write, truncate, mkdir, remove, rename and basic-attribute updates
clear all cached metadata and listings before and after the operation, including
on failure. New queries cannot join pre-mutation requests, and old responses cannot
repopulate the cache. Reads overlapping a mutation are not retained. Clearing the
whole cache covers aliases and parent metadata at the cost of hit rate during
writes. The cache is not shared across mounts or restored after disconnect.

The opt-in mount statistics described in command help report cumulative SFTP
request counts, completed-request latency totals/maxima in nanoseconds, framed
byte counts, error statuses (excluding EOF), current/peak outstanding requests,
cache hits/misses/coalesced queries/evictions and retained cache estimates. Windows
also measures Open, Create, Close, Rename, ReadDirectory, GetFileInfo and
SetBasicInfo callbacks, including lock wait time. Nested callback timings overlap
and must not be summed as total wall time. Reports contain no paths, credentials
or file contents. They go to stderr (the worker log for background mounts); the
final snapshot is emitted when a mount with initialized diagnostics exits.

Windows handle opens may run concurrently over SFTP; rename and path-based
attribute updates wait for these opens before changing handle/path bookkeeping.
Close removes its tracked path under that lock, then delegates remote close without
holding it. Synthetic-only basic-attribute updates reuse the handle validation's
metadata instead of fetching it again. Background mount and forward SSH transports
inherit their worker's hidden Windows console. OpenSSH-created console askpass
children inherit the same hidden console, avoiding a fresh visible window while
the launcher collects answers through its temporary broker. Foreground mounts
retain console authentication.

Each outstanding SFTP request has its own deadline; sibling responses do not
extend it. Timeout closes the transport and fails pending operations without
claiming that remote writes were cancelled. Startup timeout is separately opt-in
and includes authentication. Idle mounts have no request timer. Unmount normally
runs while the transport is live; busy mounts stay available for retry, while a
second interrupt or shutdown timeout forces disconnection. See
[SFTP mount safeguards and limits](SECURITY.md#sftp-mounts).

## Inventory and diagnostics

Local status separates worker liveness, transport state and unprobed target health.
Text inventory combines shells and resources in one task-ID-ordered table with
`ID`, `TASK`, `HOST`, `STATE` and `DETAIL` columns. It distinguishes cached shell state and
confirmation age separate from local connection status. Successful queries emit
no progress log; diagnostic fields remain under `--verbose`.
Resource and local shell status queries each use at most eight workers and a
two-second aggregate deadline. Local shell probes authenticate against the exact
record snapshot rather than resolving the name again during the query.
`list --verbose` and `list --json` include recent resource results; JSON uses an
explicit field allowlist. All remote inventory requires a matching release
and never triggers installation. CLI help defines the output contract and errors.

Peer records are advisory mirrors, not resources or permissions. Mounts publish
through SFTP; forwards use a same-release watcher when available and otherwise
keep the primary forwarding transport working. Sync failure does not fail the
resource. Records live in the remote account's `.local/state/sshore/peer-tasks`;
a custom SFTP root may produce an inventory not visible to the shell. Refresh is
about every 20 seconds; after 75 seconds a missing heartbeat becomes `unknown`.
Pending mirrors expire after ten minutes, other stale mirrors after 24 hours.

Local resource records/logs and remote shell runtime state are separate. Use
`list --verbose` for resource log locations. Remote shell logs live under
`$XDG_RUNTIME_DIR/sshore` or `/tmp/sshore-UID`, with `SSHORE_RUNTIME_DIR` overriding
the runtime directory. Shell exit records and local resource receipts are bounded
to 100 per category for seven days. They do not contain terminal replay or prove
that every task ended; forced termination and runtime cleanup can leave no record.

## Build and validate

Use the normal shared toolchain required by `go.mod`; check `command -v go` and
`go version` first. Do not lower requirements or create a private toolchain to
make checks pass. Build a development binary with:

```sh
go build -o dist/sshore .
```

For routine changes, run the relevant tests with `-run`; use the core suite
when changes span components:

```sh
go test -tags=sshoretest -timeout=5m ./...
# Optional native mount smoke with the existing FUSE/WinFsp installation:
SSHORE_TEST_FTP_MOUNT=1 go test -tags=sshoretest -run '^TestFTPNativeMount$' ./internal/files
```

Use `go vet ./...` for structural changes and add `-race` for concurrency changes.
Run installer checks only when installation or packaging changes. Real SSH
lifecycle tests are needed for transport/setup changes and must use an isolated
tagged executable:

```sh
go build -tags=sshoretest -o dist/sshore-e2e .
python3 tests/ssh_e2e.py --binary dist/sshore-e2e
```

Never run simulated upgrade confirmation against the developer's real shells.
Release binaries must not use `sshoretest`. Native mount tests are opt-in and
require the platform mount environment; cross-compilation does not validate it.
The manually triggered Tests workflow runs core Go checks on Linux/Windows.
Its `extended` option adds race detection, installer checks and Linux SSH
integration. Build validation remains in release CI; do not repeat full checks
after a successful run unless further changes warrant it.

The local SFTP concurrency
benchmark in `sftp_benchmark_test.go` is an in-process baseline, not WAN performance
evidence. Do not tune production defaults from that result alone.

For documentation changes, check relative links, Markdown fences and affected
Mermaid diagrams. README depicts user flow; SECURITY depicts trust boundaries.
Keep numeric defaults in CLI help and their implementation unless a design or
security explanation needs them.

## Release

`VERSION` and the source default in `main.go` must agree and use a full semantic
version. Put version-specific changes and migration notes in `scripts/release.sh`.
Do not accumulate a changelog in the project documents.

CI builds Linux and Windows amd64 in one Linux job on pushes and pull requests,
sharing checkout, Go setup and dependency caches. It packages both targets and
uploads one artifact. On `main`, after both builds succeed, the release job checks that the commit is still the branch
head, creates the new tag, and publishes archives plus `SHA256SUMS`. Builds use
`-trimpath` and version linker flags without `sshoretest`. Published Git tags are immutable; existing release assets are never overwritten.
After publication, `scripts/prune_releases.py` plans retention from a complete
paginated release snapshot: each canonical `x.y` series keeps its highest published
patch, and older release objects/assets are deleted only when that series has a
complete Linux/Windows amd64 replacement and checksums. Drafts, prereleases and
noncanonical tags are skipped. Failed publication or missing new assets prevents
cleanup. Existing-version reruns may finish cleanup without replacing assets. Full
tests are run separately and are not an automatic release gate.

Archives contain the executable, project documents and license notices. Verify
release status, commit/tag identity and both platform assets after publishing.
A source-only change at an already published version does not replace that release.

The compact list groups sessions by execution host: shells by their host and
mounts/forwards by the computer managing their process. Local sessions appear
first, followed by other host groups in lexical order; stable IDs sort each group.
REMOTE shows the known connection target, with no inferred peer path or port.
Peer mirrors retain read-only and uncertain states; verbose output adds full
identifiers and descriptions. Locally inspected resources carry a presentation-only
origin marker to distinguish them in mixed connection inventories. JSON identities
and numeric IDs remain unchanged.

### Shell ownership and attachment sources

`pstree.go` owns shell relationship rendering and inventory reconciliation;
`pstree_state.go` owns bounded, private display snapshots. Attach handshakes
carry up to 64 nodes identified by immutable shell identities, never by host
labels or PIDs. Initial startup fixes the lifetime policy and, for spawns,
the owner context. Detach and reattachment preserve these fields. The attachment
source changes on attach and clears on detach; it never transfers ownership.
The original owner context remains available after attachment elsewhere.

Tree branches represent spawn owner contexts. Persistent launches are roots,
even when entered from another managed shell; an `attach via` annotation shows
that interaction separately. Unknown/external owners remain at root. The OS
parent PID may belong to a wrapper rather than the displayed managed context.
These snapshots never participate in authorization or process signaling.
Shells inherit only their own lookup identity, so subsequent commands read
updated relationships. Cross-host snapshots are cached observations; live
inventory refreshes shell state by instance identity without duplicating nodes.
It does not verify relationship metadata. Rendering guards cycles, and bounded
snapshot validation rejects duplicate/self identities and oversized fields.

`list` shows a LIFETIME column (also `lifetime` in shell JSON): `parent`,
`heartbeat`, or `persistent`. This describes startup policy, not heartbeat
health. A detached remote spawn still has its independent lifetime connection;
a persistent launch requires no client heartbeat. The OS process tree in
`list --verbose` remains actual same-user process ancestry, separate from
`pstree`'s managed shell relationships.

`sshore pstree HOST` queries that account's known tree through the existing exact
release inventory endpoint. Colors use the terminal palette and respect
redirection, `NO_COLOR` and `TERM=dumb`. The current node has a textual marker
as well as color. Display labels are sanitized before terminal output.
