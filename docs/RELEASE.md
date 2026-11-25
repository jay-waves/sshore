# Release

1. Update migration consequences in the [publisher](../scripts/publish.go) when needed.
2. Run the relevant checks and push to `main`. [CI](../.github/workflows/ci.yml)
   builds Linux and Windows amd64 archives and checksums as short-lived artifacts.
3. At the intended commit, run `git tag vX.Y.Z` and
   `git push origin vX.Y.Z`. [Publish](../.github/workflows/publish.yml)
   builds and publishes both archives and their `SHA256SUMS` in one command,
   using the pushed tag as the binary version.
4. Verify the published commit/tag, both archives and their checksums before
   pruning older releases in the same major/minor series. Retain the highest
   published patch and all Git tags. Exclude drafts, prereleases and noncanonical
   tags from pruning. Published tags and assets are immutable.

Run `go run ./scripts` to build development packages using the nearest
full release tag for managed connections. The [single-binary build](BUILD.md)
reports `dev`.
