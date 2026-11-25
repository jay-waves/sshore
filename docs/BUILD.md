# Build and validate

Use Go and Git from the shared PATH and check the Go version against
[go.mod](../go.mod). From the repository root, run
`go run ./scripts go build -o dist/sshore .` on Linux or target
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
validation. Keep permanent integration scenarios when they protect a shared
lifecycle or trust boundary such as shell ownership, release matching,
file integrity or explicit approval. Exercise those contracts through the
CLI or installer in isolation. OS-specific validation uses the affected OS
and its real terminal or mount driver. Keep real account shells out of tests
that simulate upgrade confirmation or reset.

- `tests/integration/ssh_docker.sh` runs the Linux installer and the built
  Linux client in a disposable Alpine container with `/dev/fuse`. The client
  and OpenSSH server run under separate Linux accounts. Mount scenarios cover
  open handles across replacement, file I/O, directory operations, remote
  changes, symlink handling, read-only mounts, and unmount.
- On a Linux amd64 host with QEMU and `/dev/kvm`, run the same fixture in an
  Alpine guest with `tests/integration/ssh_docker.sh --kvm`. Either mode accepts
  a Go test pattern, for example `'^TestOpenSSH$/^mount'`.
- The Windows Server 2025 CI job checks installation on PowerShell 5.1 and 7,
  local ConPTY, and remote shell creation, detach, rename, reattach, exit, and
  kill through Windows OpenSSH under a separate account.
  A WSL client also tests Windows SFTP uploads, downloads and no-overwrite behavior.

The [Tests workflow](../.github/workflows/tests.yml) runs on demand and on pushes
to `main`.
