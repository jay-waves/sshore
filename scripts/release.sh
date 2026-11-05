#!/usr/bin/env bash
# Publish a full semantic version after all main-branch CI jobs pass.
# Existing version tags never move. A retry at the same commit can finish asset
# uploads; later main commits with an unchanged VERSION do not replace a release.
set -euo pipefail

version=$(cat VERSION)
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
tag="v$version"

head=$(gh api "repos/$GH_REPO/git/ref/heads/main" --jq '.object.sha')
if [[ "$head" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: this commit is no longer the main branch head."
  exit 0
fi

existing=$(gh api "repos/$GH_REPO/git/matching-refs/tags/$tag" \
  --jq ".[] | select(.ref == \"refs/tags/$tag\") | .object.sha")
if [[ -n "$existing" && "$existing" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: $tag already belongs to another commit; bump VERSION to publish again."
  exit 0
fi
if [[ -z "$existing" ]]; then
  gh api --method POST "repos/$GH_REPO/git/refs" \
    -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" > /dev/null
fi

cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<NOTES
sshore $version

Includes Linux and Windows binaries for amd64 and arm64.
Verify downloaded archives against SHA256SUMS.
Commit: $GITHUB_SHA
NOTES
if [[ "$version" == "0.3.0" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Dynamic same-port loopback forwarding: `sshore forward PORT`, `--list`, and `--stop PORT`.
- Forwarding belongs to the current connection; detach/disconnection closes listeners and connections.
- Native SSH forwarding and diagnostics: `-L`, `-R`, `-D`, `-C`, `-E`, and `-vv`/`-vvv`.
- Attached short-option values such as `-p2222` and `-R9000:localhost:3000`.

Upgrade: protocol 3 requires matching client and server binaries. Exit old
persistent sessions with the old client before upgrading; old daemons cannot
upgrade in place. Linux servers; Linux and Windows clients.
NOTES
fi


if [[ "$version" == "0.3.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Remote file and port requests now queue a local approval and detach, preserving the remote shell.
- Run `sshore approve` locally to review and execute once; SSH authentication remains with system OpenSSH.
- `--attach` (default) resumes the remote shell; `--stay` stays local, keeping port tunnels in the foreground until Ctrl-C.
- List or reject pending requests with `approve --list` and `approve ID --reject`.
- Requests expire after 10 minutes. Unsolicited file/port operations cannot bypass approval; `--download-dir` only sets defaults.
- Added SECURITY.md with the trust model, attacker assumptions, protections, and remaining limits; included in release archives.

Upgrade: despite the patch version, this release changes the wire protocol to 4.
Update both client and server. Exit existing sessions with the old client before
upgrading; running protocol-3 daemons cannot upgrade in place. Batch downloads
now require one approval and execute sequentially, stopping on the first failure.
NOTES
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
