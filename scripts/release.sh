#!/usr/bin/env bash
# Maintain one rolling release per version line after all CI jobs pass.
set -euo pipefail

series=$(cat VERSION)
[[ "$series" =~ ^0\.(0|[1-9][0-9]*)$ ]]
tag="v$series"

# An older manual rerun must not replace binaries from a newer main commit.
head=$(gh api "repos/$GH_REPO/git/ref/heads/main" --jq '.object.sha')
if [[ "$head" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: this commit is no longer the main branch head."
  exit 0
fi

cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<EOF
Rolling release for the $series version line.

This release and its tag are updated after successful CI on main.
Commit: $GITHUB_SHA
Binary version: $series+${GITHUB_SHA:0:7}

Includes Linux and Windows binaries for amd64 and arm64.
Verify downloaded archives against SHA256SUMS.
EOF

existing=$(gh api "repos/$GH_REPO/git/matching-refs/tags/$tag" \
  --jq ".[] | select(.ref == \"refs/tags/$tag\") | .ref")
if [[ -n "$existing" ]]; then
  gh api --method PATCH "repos/$GH_REPO/git/refs/tags/$tag" \
    -f sha="$GITHUB_SHA" -F force=true > /dev/null
else
  gh api --method POST "repos/$GH_REPO/git/refs" \
    -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" > /dev/null
fi

release=$(gh release list --limit 1000 --json tagName --jq ".[] | select(.tagName == \"$tag\") | .tagName")
if [[ -n "$release" ]]; then
  gh release upload "$tag" packages/*.tar.gz packages/*.zip packages/SHA256SUMS --clobber
  gh release edit "$tag" --title "sshore $tag" --notes-file release-notes.md --latest
else
  gh release create "$tag" --verify-tag --title "sshore $tag" \
    --notes-file release-notes.md --latest \
    packages/*.tar.gz packages/*.zip packages/SHA256SUMS
fi
