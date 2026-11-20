#!/usr/bin/env bash
# Publish the pushed release tag; published tags and assets are immutable.
set -euo pipefail

version=$(cat VERSION)
tag="v$version"
if [[ "$GITHUB_REF" != "refs/tags/$tag" ]]; then
  echo "Expected refs/tags/$tag for VERSION $version, got $GITHUB_REF." >&2
  exit 1
fi

cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<NOTES
sshore $version

- Measure SSH connection and echo latency with ping.
- Simplify file endpoints and shell targets; local session selectors now require
  a prefix such as :NAME, while a bare shell target names an SSH host.
- Name shells at creation with HOST:NAME or :NAME.

Includes Linux and Windows amd64 binaries. Verify archives against SHA256SUMS.
Update both endpoints to $version for managed connections. Save work before
installing because the installer stops the target account daemon.
Remote releases before 0.7.22 do not support the current service entry. If remote
maintenance is needed before upgrading, run that release's sshore kill --all on the host.
Commit: $GITHUB_SHA
NOTES
gh release create "$tag" --verify-tag --title "sshore $tag" \
  --notes-file release-notes.md --latest \
  packages/*.tar.gz packages/*.zip packages/SHA256SUMS
