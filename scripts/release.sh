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

Includes Linux and Windows binaries for amd64 (x86-64) only.
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


if [[ "$version" == "0.3.2" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Added a user-run Linux amd64/arm64 installer: architecture detection, pinned or latest releases, HTTPS downloads, SHA256 verification, atomic installation, and old-version backup.
- Missing remote executables now print an installation command matching the local client. No automatic installation is performed.
- Default remote lookup falls back to ~/.local/bin/sshore when it is absent from PATH; explicit --server paths remain authoritative.
- Added installer, remote-probe, and real-SSH regression coverage, plus installation trust-boundary documentation.
- Fixed Ctrl-C in foreground `approve --stay` tunnels so normal cancellation does not race with SSH process termination.

Install on Linux:
```sh
curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/v0.3.2/install.sh | sh -s -- --version 0.3.2
```

Protocol remains 4, compatible with 0.3.1 session daemons. When upgrading from
0.3.0 or earlier, exit old sessions with the old client and update both ends.
NOTES
fi

if [[ "$version" == "0.3.3" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Clear mouse/focus reporting and bracketed paste when returning to the local terminal, including approval boundaries; preserve Unix TTY and Windows Console settings.
- Track standard mouse modes while detached or approval output is muted, restoring them when terminal interaction resumes.
- Request a foreground-job redraw on reattachment even at unchanged dimensions; approved operations wait until output is enabled, and --stay remains local.
- Keep remote output transparent, with bounded mouse tracking rather than full terminal emulation.
- Add terminal cleanup, split-sequence parsing, foreground redraw, and multiplexer reconnection regression tests.

Protocol remains 4. Update client and server and create a new remote session
for all fixes: already-running daemons retain their original code. Redraw is
best effort; terminal-specific passthrough and full screen replay are not emulated.
NOTES
fi

if [[ "$version" == "0.3.4" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add a Windows PowerShell installer for amd64/arm64, with optional INSTALL_DIR and pinned or latest release selection.
- Verify SHA256SUMS and the executable version before replacement. PATH, execution policy and PowerShell profiles remain unchanged; setup instructions are printed only.
- Document user responsibility for installation permissions, closing running executables, backups, PATH configuration and compatible client/server versions.
- Consolidate terminal recovery tests and remove emulator-specific CI dependencies, retaining protocol, approval, transfer integrity and terminal recovery coverage.
- Add offline installer checks to Windows PowerShell 5.1 and PowerShell 7 CI.

Install the latest Windows client:
```powershell
irm https://raw.githubusercontent.com/jay-waves/sshore/v0.3.4/install.ps1 | iex
```

Default directory: %LOCALAPPDATA%\sshore\bin. Set $env:INSTALL_DIR to override it.
Protocol remains 4. Running remote session daemons are not upgraded or stopped.
NOTES
fi


if [[ "$version" == "0.3.5" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add `sshore connect HOST` to connect unambiguously to SSH aliases that match subcommands.
- Fix inherited Windows mouse capture in remote shells while preserving mouse input for remote TUIs.

Protocol remains 4. Update the Windows client to receive the mouse fix.
NOTES
fi

if [[ "$version" == "0.3.6" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Add local Tab path completion to upload and download approval prompts.
- Start single-file uploads when the local source path is submitted, without a separate yes prompt; Ctrl-C cancels.
- Preserve final confirmation for downloads, forwarding and batch requests, and release the line editor before reconnecting.

Protocol remains 4. Update the local client to use the new prompts.
NOTES
fi

if [[ "$version" == "0.3.7" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Connections without -s create or resume the default main session.
- Explicit -s NAME continues to create or resume a separate named session.

Protocol remains 4. Update the client for default-session reuse.
NOTES
fi

if [[ "$version" == "0.4.1" ]]; then
  cat >> release-notes.md <<'NOTES'

Changes:
- Require one exact active version per remote account; reject newer remote versions without downgrading.
- Automatically prepare a verified remote executable when missing or older. Updating an existing installation requires explicit confirmation before ending listed old sessions and tasks.
- Offer a nonpersistent recovery shell on the same SSH connection for manual maintenance, with sshore features disabled.
- Check running daemon versions, report background sessions, and serialize managed setup operations.
- Unify status colors and UTF-8 path handling; improve default session reuse and local path input.
- Preserve previous GitHub releases and assets when publishing new versions.

Updating old sessions terminates their tasks. Press Enter at setup to keep them
and use manual recovery instead. A matching published binary must be available
for automatic preparation; source/custom builds may need manual installation.
Checksums verify downloaded bytes, not a compromised release publisher.
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
