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

- Share the shell lifecycle across local and hosted tasks and serve live inventory from a single daemon snapshot.
- Store Linux hosted-shell exit records in the private account daemon directory. Older records remain readable until they expire.
- Keep command panel results on screen and add color to the title and prompt.
- Add an experimental macOS arm64 client build from source.

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
