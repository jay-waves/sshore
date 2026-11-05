#!/bin/sh
# User-invoked installer for the Linux server/client. Never run by sshore itself.
# Usage: curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh | sh -s -- [--version 0.3.2] [--bin-dir DIR]
set -eu

fail() { printf 'sshore install: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: sh install.sh [--version X.Y.Z] [--bin-dir DIRECTORY]

Install a published Linux amd64/arm64 release, without sudo or shell profile edits.
Default: latest release into $HOME/.local/bin. Explicit --version is recommended
when matching a local client. Requires curl, tar and sha256sum (or shasum).
Existing versions are backed up as sshore-X.Y.Z before replacement.
Windows clients: download the matching .zip from GitHub Releases.
HELP
}

main() {
    install_version=
    install_bin_dir=${HOME:?HOME must be set}/.local/bin
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --version|--bin-dir)
                [ "$#" -ge 2 ] && [ -n "$2" ] || fail "missing value for $1"
                case "$1" in
                    --version) install_version=$2 ;;
                    --bin-dir) install_bin_dir=$2 ;;
                esac
                shift 2 ;;
            -h|--help) usage; return ;;
            *) fail "unknown option: $1" ;;
        esac
    done
    if [ -n "$install_version" ]; then
        valid_version "$install_version" || fail 'version must be X.Y.Z (without v)'
    fi
    [ "$(uname -s)" = Linux ] || fail 'only Linux servers/clients are supported by this script; Windows clients use release ZIPs'
    case "$(uname -m)" in
        x86_64|amd64) install_arch=amd64 ;;
        aarch64|arm64) install_arch=arm64 ;;
        *) fail 'unsupported CPU architecture (requires amd64 or arm64)' ;;
    esac
    for dependency in curl tar awk mktemp; do
        command -v "$dependency" >/dev/null 2>&1 || fail "required command missing: $dependency"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        install_hasher=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        install_hasher=shasum
    else
        fail 'required command missing: sha256sum or shasum'
    fi

    # Downloads and checksum verification precede changes to the installation.
    umask 077
    install_tmp=$(mktemp -d "${TMPDIR:-/tmp}/sshore-install.XXXXXXXX")
    install_stage=
    trap 'rm -rf "$install_tmp"; if [ -n "$install_stage" ]; then rm -f "$install_stage"; fi' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM HUP
    install_releases=https://github.com/jay-waves/sshore/releases
    if [ -n "$install_version" ]; then
        install_checks_url=$install_releases/download/v$install_version/SHA256SUMS
    else
        install_checks_url=$install_releases/latest/download/SHA256SUMS
    fi
    download "$install_checks_url" "$install_tmp/SHA256SUMS"
    if [ -z "$install_version" ]; then
        # Infer the version from a single matching asset, without jq or API quota.
        install_version=$(awk -v arch="$install_arch" '
            $2 ~ ("^sshore_[0-9]+\\.[0-9]+\\.[0-9]+_linux_" arch "\\.tar\\.gz$") {
                split($2, parts, "_"); version=parts[2]; n++
            }
            END { if (n==1) print version; else exit 1 }
        ' "$install_tmp/SHA256SUMS") || fail 'release manifest does not identify one matching Linux asset'
        valid_version "$install_version" || fail 'invalid version in release manifest'
    fi
    install_asset=sshore_${install_version}_linux_${install_arch}.tar.gz
    install_expected=$(awk -v name="$install_asset" '$2==name { hash=$1; n++ } END { if (n==1) print hash; else exit 1 }' "$install_tmp/SHA256SUMS") || fail 'missing or duplicate checksum entry'
    [ "${#install_expected}" -eq 64 ] || fail 'invalid checksum entry'
    case "$install_expected" in *[!0-9a-fA-F]*) fail 'invalid checksum entry' ;; esac
    download "$install_releases/download/v$install_version/$install_asset" "$install_tmp/archive.tar.gz"
    if [ "$install_hasher" = sha256sum ]; then
        install_actual=$(sha256sum "$install_tmp/archive.tar.gz" | awk '{print $1}')
    else
        install_actual=$(shasum -a 256 "$install_tmp/archive.tar.gz" | awk '{print $1}')
    fi
    [ "$install_actual" = "$install_expected" ] || fail 'SHA256 mismatch; installation unchanged'
    # Extract only the named payload to stdout: archive paths cannot write outside
    # the temporary directory, even if an archive contains additional members.
    tar -xzOf "$install_tmp/archive.tar.gz" sshore > "$install_tmp/sshore" || fail 'cannot extract sshore'

    # Use an absolute directory so options/relative paths cannot confuse tools.
    mkdir -p -- "$install_bin_dir"
    install_bin_dir=$(cd -- "$install_bin_dir" && pwd -P)
    install_target=$install_bin_dir/sshore
    [ ! -d "$install_target" ] || fail 'installation target is a directory'
    if [ -e "$install_target" ] || [ -L "$install_target" ]; then
        install_old=$("$install_target" --version 2>/dev/null || true)
        case "$install_old" in
            'sshore '*)
                install_old=${install_old#sshore }
                if valid_version "$install_old"; then
                    install_backup=$install_bin_dir/sshore-$install_old
                    # Do not follow or overwrite a preexisting backup, including a
                    # dangling symlink. A backup makes old daemons recoverable.
                    if [ ! -e "$install_backup" ] && [ ! -L "$install_backup" ]; then
                        cp -p -- "$install_target" "$install_backup"
                    fi
                fi ;;
        esac
    fi
    install_stage=$(mktemp "$install_bin_dir/.sshore-install.XXXXXXXX")
    cat "$install_tmp/sshore" > "$install_stage"
    chmod 755 "$install_stage"
    [ "$("$install_stage" --version)" = "sshore $install_version" ] || fail 'binary version does not match release'
    mv -f -- "$install_stage" "$install_target"
    install_stage=
    printf 'Installed sshore %s: %s\n' "$install_version" "$install_target"
    printf '%s\n' 'SHA256 verified. No sudo or shell configuration changes.'
    printf '%s\n' 'Use matching client/server versions. Existing session daemons are not upgraded or stopped.'
    case ":$PATH:" in
        *":$install_bin_dir:"*) ;;
        *) printf 'Directory is not on PATH. Use the full path, or add %s to PATH.\n' "$install_bin_dir" ;;
    esac
}

valid_version() {
    # No shell expressions, URL path components, prereleases or ambiguous names.
    printf '%s\n' "$1" | awk '/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ {ok=1} END {exit !(ok && NR==1)}'
}
download() {
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --tlsv1.2 --connect-timeout 15 --max-time 180 --retry 2 "$1" -o "$2" || fail "download failed: $1"
}

# Wrapping execution in a function prevents a partial download from running an
# incomplete installer body. The script never reads approval/passwords from stdin.
main "$@"
