# Local approval and the command console

## Local approval

A program in an attached shell can request a copy, mount or port forward through `origin:`.
The request contains bounded operation metadata and optional path or port suggestions. The
client leaves the remote attachment and presents the request in a local approval screen.
The screen also handles any SSH credentials needed to start a mount or forward.

File and mount requests show `Approve this request? [y/N]` before local endpoint
selection. For a forward, the screen shows the direction and fixed remote endpoint.
Submitting a valid local port approves that mapping. Suggested values are editable,
and path completion leaves the decision open.
For a mount without a local suggestion, press Enter to confirm the current directory,
`a` to allocate a mountpoint, or `:` to type a path.

Cancel, EOF or lack of an interactive terminal rejects the request without executing it.
Grants are single-use, and there is no pending request queue. Unsolicited transfer offers,
forward-add streams and reverse connection streams remain rejected.

Approval fixes the selected local endpoints. Startup revalidates the original detached
shell identity and exact release. A mount or forward consumes its private handoff ID for
that task and requested operation. All file endpoints in a batch are selected before
execution. An approved resource uses the scoped route described in the
[mount implementation](mount.md) and [network topology](NETWORK.md).

Remote output can imitate the local screen, and processes sharing a remote account can
submit requests. A request provides no evidence of who typed it. Verify the context before
entering credentials or granting access. Private records are within the local account's
trust boundary. See the [security model](SECURITY.md#trust-boundaries).

## Command console

`Ctrl+B s` or `Ctrl+B :` opens the local `sshore>` command console from an active shell.
The frontend temporarily owns input and screen output while the console is open. Terminal
output is buffered while the console is active. Leaving restores the application screen
and input modes.

`lf` opens a directory picker. In an attached shell it starts at the reported working
directory and requests listings on the existing authenticated control channel. Confirming
a directory starts a child shell through the normal `sh` handoff. Cancelling returns to
the console. With no shell attached, it browses locally and opens a local shell.
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
