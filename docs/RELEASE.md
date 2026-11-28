# Release

1. Record release changes and migration instructions in the [changelog](../CHANGELOG.md).
2. Run the relevant checks and push to `main`. [CI](../.github/workflows/ci.yml)
   builds Linux and Windows amd64 archives and checksums as short-lived artifacts.
3. At the intended commit, run `git tag vX.Y.Z` and
   `git push origin vX.Y.Z`. [Publish](../.github/workflows/publish.yml)
   builds both archives and publishes them with their `SHA256SUMS`,
   using the pushed tag as the binary version.
4. Verify the published commit/tag, both archives and their checksums. Retain
   published tags and assets unchanged. A client can need either archive from
   its exact release to deploy to a new host.

Run `go run ./scripts` to build development packages using the nearest
full release tag for managed connections. The [single-binary build](BUILD.md)
reports `dev`.
