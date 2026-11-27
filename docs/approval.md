# Local approval and the command console

## Local approval

A program in an attached shell can request a copy, mount or port forward through `origin:`.
The request contains bounded operation metadata and optional path or port suggestions. The
client leaves the remote attachment and presents the request in a local approval screen.
The screen also handles any SSH credentials needed to start a mount or forward.
In an attached shell, use `: origin: --lf` with `cp` or `mount` to browse the host
before requesting approval. `.:` and `here:` also select the current host.
Use `cp origin: : --lf` to browse for the source on the client after approving.

File and mount requests show `Approve this request? [y/N]` before local endpoint
selection. For a forward, the screen shows the direction and fixed remote endpoint.
Submitting a valid local port approves that mapping. The remote port is prefilled when the
request omits the local port. For a mount, the suggested mountpoint is editable and `.:` is
prefilled when no location was suggested. Use `--lf` to browse for a mountpoint.

Cancel, EOF or lack of an interactive terminal rejects the request without executing it.
Grants are single-use, and there is no pending request queue. Unsolicited transfer offers,
forward-add streams and reverse connection streams remain rejected.

Approval fixes the selected local endpoints. Each routed operation revalidates the original
detached shell identity and exact release, matches the stored operation, and consumes it
before starting work. All file endpoints in a batch are selected before execution.
With `cp -r`, the screen shows the requested directory tree and asks for its local
source or destination. A single grant covers that tree. The remote endpoint opens
the approved directory as its root and allows reading for downloads or creating
directories and new files for uploads.
An approved resource uses the scoped route described in the
[mount implementation](mount.md) and [network topology](NETWORK.md).

Remote output can imitate the local screen, and processes sharing a remote account can
submit requests. A request provides no evidence of who typed it. Verify the context before
entering credentials or granting access. Private records are within the local account's
trust boundary. See the [security model](SECURITY.md#trust-boundaries).

## Standalone browsers

`sshore lf` opens the current directory. Give it a host or a host-qualified path to browse over
SFTP. Use `s` to open a shell at the selected directory, `c` to copy, or `m` to mount. On the local
machine, select an empty directory to use as the mountpoint. On a remote host, select the directory
to mount. Selecting a directory for copy includes its contents.

`f` opens the port list for the same host. Use `sshore lp [HOST]` to open that list directly.
It shows IPv4 wildcard and loopback TCP listeners with process information when available. Select
a port with the arrow keys, filter with `/`, or enter a port with `:`. Submit the target port
to start the forward. Both browsers restore the terminal before running the chosen command.
Requests sent to `origin:` follow the same approval flow as the corresponding CLI command.

## Command console

`Ctrl+B s` or `Ctrl+B :` opens the local `sshore>` command console from an active shell.
The frontend temporarily owns input and screen output while the console is open. Terminal
output is buffered while the console is active. Leaving restores the application screen
and input modes.

`lf` opens a directory picker. In an attached shell it starts at the reported working
directory and requests listings on the existing authenticated control channel. Confirming
a directory starts a child shell through the normal `sh` handoff. While browsing, the
frontend routes input and screen output to the picker and pauses the command editor.
Cancelling restores the command prompt. With no shell attached, `lf` browses locally
and opens a local shell.
Windows hosted SSH shells currently lack nested routes.

An explicit console command uses the existing shell or task-control rules. Resource
requests from the attached shell still require the local approval flow above. Keep
console input, remote terminal bytes and approval messages on their existing paths.
Render remote names and errors as text before adding terminal styling.

## Implementation and checks

- [Local review](../review.go), [single-use requests](../review_store.go) and
  [route authorization](../resource_route_host.go) implement consent and scope.
- [Console routing](../client_console.go), [directory requests](../console_files.go)
  and [terminal ownership](../client.go) keep commands and screen rendering local.
- [OpenSSH integration scenarios](../tests/integration/openssh_linux_test.go) cover
  declined transfers, approved resources outliving their shell, and `lf` launching a shell.
