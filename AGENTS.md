# Contributor constraints

- Prefer the simplest direct implementation and workflow. Avoid speculative defenses, fallback paths and redundant checks; the publisher handles release failures. Preserve the explicit security and release invariants below.
- Support multi-platform clients and Unix-like controlled hosts. Currently supported clients are Linux and Windows amd64; Linux amd64 is the supported controlled host. Windows remote hosting, split panes and tiling are out of scope. See [platform and terminal scope](SSHORE.md#terminal-and-mux-design-scope).
- Use the account's authenticated daemon and registered tasks as the managed scope. Validate process identity before signaling; never kill by process name or adopt unregistered processes. Keep normal connections on exact full release versions. Preserve the update, reset and approval safeguards in [SECURITY.md](SECURITY.md).
- Keep published Git tags immutable. Follow the [release workflow](SSHORE.md#release), including verified assets before pruning old releases.
- Use the shared toolchain from the normal PATH. Check versions against repository requirements; do not substitute private runtimes, lower requirements or install tools without authorization. For browser checks, reuse global Playwright and system Chrome; do not download browsers.
- Keep unit tests for core, side-effect-free code. Verify ordinary features with temporary checks and discard them after use. Keep lasting system contracts in external integration tests. Follow [Build and validate](SSHORE.md#build-and-validate).
- Define CLI grammar once; parsing and help must not start services or change state. Keep destructive consequences and confirmations clear.
- Keep project documentation in the five root documents and update the responsible document when behavior changes. See [documentation ownership](SSHORE.md#documentation-ownership).
