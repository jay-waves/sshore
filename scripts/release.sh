#!/usr/bin/env bash
# Publish a full semantic version after both main-branch release builds pass.
# CI builds only Linux/Windows amd64. Full validation is
# available locally or through the manual Tests workflow, independently of release.
# Published tags never change. Existing releases are never overwritten.
# After successful publication, retain only the latest complete patch per x.y.
set -euo pipefail

version=$(cat VERSION)
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
tag="v$version"

head=$(gh api "repos/$GH_REPO/git/ref/heads/main" --jq '.object.sha')
if [[ "$head" != "$GITHUB_SHA" ]]; then
  echo "Skipping release: this commit is no longer the main branch head."
  exit 0
fi

existing_ref=$(gh api "repos/$GH_REPO/git/matching-refs/tags/$tag" \
  --jq ".[] | select(.ref == \"refs/tags/$tag\") | .object.sha")
if [[ -n "$existing_ref" ]]; then
  # The ref SHA is a tag-object SHA for annotated tags. The commits endpoint
  # dereferences both annotated and lightweight tags to their commit.
  existing_commit=$(gh api "repos/$GH_REPO/commits/$tag" --jq '.sha')
  if [[ "$existing_commit" != "$GITHUB_SHA" ]]; then
    echo "Skipping release: $tag already belongs to another commit; bump VERSION to publish again."
    exit 0
  fi
else
  gh api --method POST "repos/$GH_REPO/git/refs" \
    -f ref="refs/tags/$tag" -f sha="$GITHUB_SHA" > /dev/null
fi

cat packages/*.sha256 > packages/SHA256SUMS
cat > release-notes.md <<NOTES
sshore $version

Includes Linux and Windows binaries for amd64 (x86-64) only.
Verify downloaded archives against SHA256SUMS.
Commit: $GITHUB_SHA
NOTES
if [[ "$version" == "0.7.12" ]]; then
  cat >> release-notes.md <<'NOTES'

- Linux PTY and MCP Bash sessions use pollable process exit waits when available,
  reducing OS threads with many idle sessions.
- Linux SFTP mounts use 64 KiB chunks to reduce read and write requests.
- Windows ConPTY uses native synchronous child pipe handles, overlapped daemon
  I/O and thread-pool process waits. Shell exit codes follow drained output.
- ProxyJump hops inherit explicitly supplied SSH configuration files.
- Windows OpenSSH known_hosts lists support multiple drive-qualified paths.
- Ctrl+C aborts SSH password and keyboard-interactive prompts without retrying.

Update both endpoints to 0.7.12 for managed connections. Save work before
installing because the installer stops the target account daemon.
NOTES
fi

release=$(gh release list --limit 1000 --json tagName --jq ".[] | select(.tagName == \"$tag\") | .tagName")
if [[ -n "$release" ]]; then
  echo "Release $tag already exists; keeping its assets and checking retention."
else
  gh release create "$tag" --verify-tag --title "sshore $tag" \
    --notes-file release-notes.md --latest \
    packages/*.tar.gz packages/*.zip packages/SHA256SUMS
fi

# Fetch every page before planning deletions. Failed publication or incomplete
# replacement assets must never remove a working release. Tags are untouched.
retention_dir=$(mktemp -d)
trap 'rm -rf "$retention_dir"' EXIT
gh api --paginate --slurp "repos/$GH_REPO/releases?per_page=100" > "$retention_dir/releases.json"
python3 "$(dirname "$0")/prune_releases.py" "$tag" < "$retention_dir/releases.json" > "$retention_dir/remove.tsv"
while IFS=$'\t' read -r release_id old_tag; do
  echo "Removing superseded release $old_tag (keeping its Git tag)."
  gh api --method DELETE "repos/$GH_REPO/releases/$release_id"
done < "$retention_dir/remove.tsv"
