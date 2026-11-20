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

- Reduce local frontend-to-daemon multiplexing overhead for bulk streams.
- Refine SSH workflow fixtures and isolate benchmarks under tests/benchmarks.

Includes Linux and Windows amd64 binaries. Verify archives against SHA256SUMS.
Update both endpoints to $version for managed connections. Save work before
installing because the installer stops the target account daemon.
Commit: $GITHUB_SHA
NOTES
gh release create "$tag" --verify-tag --title "sshore $tag" \
  --notes-file release-notes.md --latest \
  packages/*.tar.gz packages/*.zip packages/SHA256SUMS
