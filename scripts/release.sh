#!/usr/bin/env bash
# Publish the current main commit once; published tags and assets are immutable.
set -euo pipefail

version=$(cat VERSION)
tag="v$version"
head=$(gh api "repos/$GH_REPO/git/ref/heads/main" --jq '.object.sha')
if [[ "$head" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: this commit is no longer the main branch head."
  exit 0
fi

existing=$(gh api "repos/$GH_REPO/git/matching-refs/tags/$tag" \
  --jq ".[] | select(.ref == \"refs/tags/$tag\") | .ref")
if [[ -n "$existing" ]]; then
  echo "Skipping release: $tag already exists; published tags are immutable."
  exit 0
fi

gh api --method POST "repos/$GH_REPO/git/refs" \
  -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" > /dev/null
cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<NOTES
sshore $version

- Rename the current-machine endpoint from self: to here: (.: remains available).
- Run command-panel inventory on the current session host, add CLI aliases, and remove panel cp and forward.
- Consolidate remote service requests and begin shared Unix and macOS arm64 client groundwork; macOS is not yet supported.

Includes Linux and Windows amd64 binaries. Verify archives against SHA256SUMS.
Update both endpoints to $version for managed connections. Save work before
installing because the installer stops the target account daemon.
Older remote releases do not support the new service entry. If remote maintenance
is needed before upgrading, run that release's sshore kill --all on the host.
Commit: $GITHUB_SHA
NOTES
gh release create "$tag" --verify-tag --title "sshore $tag" \
  --notes-file release-notes.md --latest \
  packages/*.tar.gz packages/*.zip packages/SHA256SUMS
