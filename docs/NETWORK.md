# Command network topology

## SSH commands

| Command | Transport | Startup exchange |
| --- | --- | --- |
| `shell`, `new`, `attach` | The local daemon owns the SSH connection. Each attachment uses separate SSH TCP channels for control and terminal data. | An exec channel selects or creates the remote task. The daemon validates the version and data-channel token in the attach request. |
| `sh HOST -- COMMAND` | One SSH exec channel. | SSH starts the command and returns its exit status. |
| `cp` | One SSH exec channel and one SFTP session for the entire batch. | The service returns the version and remote home, then SFTP negotiates its protocol. |
| `mount` | One SSH connection with sibling exec channels for SFTP and optional filesystem notifications. | Each service request selects data or notifications and gets one mount readiness reply. The data reply includes the account identity and remote home. |
| `forward` | One SSH exec channel per connection attempt. A smux session carries one stream per forwarded TCP connection and a control stream. | The service reports readiness after opening the required remote listener. Reconnect attempts establish a new SSH connection. |
| `ping` | An authenticated SSH connection for connection timing and echo probes. | Echo batches reuse one exec channel. |
| `copy-id` | An SSH exec operation against the selected account. | The command uses SSH authentication to install the selected public key. |

The local frontend and its daemon share a loopback connection with smux for
remote shell setup and relaying. Over SSH, separate control and terminal channels
give each path its own flow-control window. Reattaching through a live route can
reuse its authenticated connection. An independent shell setup can establish
a new SSH connection.

Omitted file paths open a three-column picker after SSH authentication.
The header identifies the host and directory being browsed. Use `gg` and `G`
to jump to the top or bottom of the current list. Use `/` or `?` to search
filenames forward or backward. Use `n` to repeat in the same direction and
`N` to repeat in reverse. In a directory picker, Enter
chooses the highlighted directory and `./` chooses the directory being browsed.
Use `l` or the right arrow to enter a highlighted directory. In the copy source
picker, Space marks or unmarks a file and moves down. A check mark identifies
marked files, and the footer shows the total across directories. Enter submits the marked
files, or chooses the highlighted file when none are marked.
In a local picker that owns the terminal, `e` opens the highlighted file with
`EDITOR` and `i` opens it with `PAGER`. Missing settings are shown in the picker.
The client starts by reading the remote home into its directory cache. A mount
worker returns the current and parent listings together over the startup
stream and reuses that cache when it mounts the selected directory. The shell
console's `lf` requests the same pair over its existing control channel.

## Approved resources

When a remote shell requests access to the origin account, the origin daemon
checks the request and uses the authorized route. A mount or forward opens an
independent SSH connection along that route so the resource can outlive the
shell. Intermediate hosts relay the resource stream. An approved mount uses
smux for its data and notification streams and validates the approval on each.
An approved forward uses the same per-connection smux streams as a direct
forward. An approved copy uses an authorized file stream for SFTP.

## Task and account commands

`list` and `pstree` read local daemon inventory and cached remote observations.
`detach` updates the active task through its managing daemon. `kill ID` and `kill NAME`
resolve a known task and contact its managing daemon. `kill HOST --all` uses an
administrative SSH exchange to preview
and confirm shutdown. `daemon` and `reset` operate on the local account.
`bootstrap` downloads the release over HTTPS and replaces the local installation.

See [architecture](ARCHITECTURE.md), [security](SECURITY.md), [approval](approval.md),
[mount implementation](mount.md) and [build and validation](BUILD.md).
