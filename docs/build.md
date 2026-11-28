# Build and validate

Use Go and Git from the shared PATH and check the Go version against
[go.mod](../go.mod). From the repository root, run
`go run ./scripts go build -o dist/sshore ./cmd/sshore` on Linux or target
`dist/sshore.exe` on Windows. The script applies the [SFTP](../scripts/patches/pkg-sftp-v2.patch)
and [WinFsp](../scripts/patches/go-winfsp.patch) patches to temporary copies of
the pinned modules and builds in a temporary Go workspace. Use the same entry
point for tests and vet.

The permanent unit suite covers terminal byte handling, SSH option parsing
and filtering of terminal output. Run it with `go run ./scripts go test ./...`.
Go tests that drive a built product or installer live under `tests/integration`
and use the `integration` build tag. Installer checks on Windows use the
[PowerShell harness](../tests/install_windows_test.ps1).

Use an ad hoc check for ordinary feature work. Keep scripts, temporary
regressions and fixture files outside the checkout and discard them after
validation. Read the permanent integration scenarios as examples of the
product's contracts. Name the behavior each scenario demonstrates and keep
the action and observable result together. Explain setup that establishes
an ownership or trust boundary. Exercise those contracts through the CLI or
installer in isolation. OS-specific validation uses the affected OS
and its real terminal or mount driver. Keep real account shells out of tests
that simulate upgrade confirmation or reset.

Start with these scenarios when reviewing a design change:

| Contract | Integration examples |
| --- | --- |
| Help, argument rejection, and recovery leave task state untouched | [CLI](../tests/integration/cli_test.go) |
| Detach preserves shell state, attach resumes it, and shutdown ends it | [ConPTY](../tests/integration/client_windows_test.go) |
| Confirming a launch preserves argument boundaries, including empty arguments | [Program lifecycle](../tests/integration/run_lifecycle_windows_test.go) |
| SSH trust, exact release matching, consent, and file integrity | [Linux OpenSSH](../tests/integration/openssh_linux_test.go), [Windows OpenSSH](../tests/integration/openssh_windows_test.go) |

- `tests/integration/ssh_docker.sh` runs the Linux installer and the built
  Linux client in a disposable Alpine container with `/dev/fuse`. The client
  and OpenSSH server run under separate Linux accounts. Mount scenarios cover
  open handles across replacement, file I/O, directory operations, remote
  changes, symlink handling, read-only mounts, and unmount.
- On a Linux amd64 host with QEMU and `/dev/kvm`, run the same fixture in an
  Alpine guest with `tests/integration/ssh_docker.sh --kvm`. Either mode accepts
  a Go test pattern, for example `'^TestOpenSSH$/^mount'`.
- The Windows Server 2025 CI job checks installation on PowerShell 5.1 and 7,
  local ConPTY, and remote shell creation, detach, reattach, exit, and
  kill through Windows OpenSSH under a separate account.
  A WSL client also tests Windows SFTP uploads, downloads and no-overwrite behavior.
- On Windows with WinFsp installed, run
  `go run ./scripts go test -tags integration -run '^TestWindowsMount$' ./tests/integration`
  to check native drive and directory mounts against a local SFTP fixture.

The [Tests workflow](../.github/workflows/tests.yml) runs on demand and on pushes
to `main`.
