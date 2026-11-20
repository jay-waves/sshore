#!/bin/sh
# Linux amd64 release installer.
# Downloads HTTPS assets, verifies SHA256SUMS and version, then atomically installs.
# Requires curl, tar and sha256sum/shasum. Default: ~/.local/bin; --bin-dir overrides.
# No sudo or shell-profile edits. Upgrades stop the account daemon after any task confirmation.
# Usage: curl -fsSL https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh | sh -s -- [--version 0.3.2] [--bin-dir DIR]
set -eu

fail() { printf 'sshore install: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: sh install.sh [--version X.Y.Z] [--path PATH | --bin-dir DIRECTORY]

Install a published Linux amd64 release, without sudo or shell profile edits.
Default: latest release into $HOME/.local/bin. Explicit --version is recommended
when matching a local client. Requires curl, tar and sha256sum (or shasum).
Upgrades stop the account daemon. If tasks are listed, follow the confirmation prompt.
Windows clients: use install.ps1 or the matching release ZIP.
HELP
}

main() {
    install_version=
    install_bin_dir=
    install_path=
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --version|--bin-dir|--path)
                [ "$#" -ge 2 ] && [ -n "$2" ] || fail "missing value for $1"
                case "$1" in
                    --version) install_version=$2 ;;
                    --bin-dir) install_bin_dir=$2 ;;
                    --path) install_path=$2 ;;
                esac
                shift 2 ;;
            -h|--help) usage; return ;;
            *) fail "unknown option: $1" ;;
        esac
    done
    if [ -n "$install_path" ]; then
        [ -z "$install_bin_dir" ] || fail '--path and --bin-dir cannot be combined'
        case "$install_path" in /*) ;; *) fail '--path must be absolute' ;; esac
        install_bin_dir=$(dirname -- "$install_path")
        install_name=$(basename -- "$install_path")
    else
        install_bin_dir=${install_bin_dir:-${HOME:?HOME must be set}/.local/bin}
        install_name=sshore
    fi
    if [ -n "$install_version" ]; then
        valid_version "$install_version" || fail 'version must be X.Y.Z (without v)'
    fi
    [ "$(uname -s)" = Linux ] || fail 'only Linux servers/clients are supported by this script; Windows clients use install.ps1 or release ZIPs'
    case "$(uname -m)" in
        x86_64|amd64) install_arch=amd64 ;;
        *) fail 'unsupported CPU architecture (requires amd64; ARM is not supported)' ;;
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
    trap 'rm -rf "$install_tmp"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM HUP
    install_releases=https://github.com/jay-waves/sshore/releases
    if [ -n "$install_version" ]; then
        install_checks_url=$install_releases/download/v$install_version/SHA256SUMS
    else
        install_checks_url=$install_releases/latest/download/SHA256SUMS
    fi
    if [ -z "$install_version" ]; then
        download "$install_checks_url" "$install_tmp/SHA256SUMS" 262144
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
    [ -f "$install_tmp/SHA256SUMS" ] || download "$install_checks_url" "$install_tmp/SHA256SUMS" 262144
    install_archive=$install_tmp/$install_asset
    download "$install_releases/download/v$install_version/$install_asset" "$install_archive" 67108864
    verify_archive "$install_tmp/SHA256SUMS" "$install_archive" || fail 'SHA256 mismatch or invalid checksum entry'
    entry=$(LC_ALL=C tar -tvzf "$install_archive" -- sshore) || fail 'cannot inspect release archive'
    case "$entry" in -* ) ;; *) fail 'release executable must be a regular file' ;; esac
    [ "$(printf '%s\n' "$entry" | wc -l)" -eq 1 ] || fail 'duplicate release executable'
    (ulimit -f 65536; tar -xzOf "$install_archive" -- sshore > "$install_tmp/sshore") || fail 'cannot extract sshore'
    [ -s "$install_tmp/sshore" ] && [ "$(wc -c < "$install_tmp/sshore")" -le 67108864 ] || fail 'invalid release executable size'

    # Use an absolute directory so options/relative paths cannot confuse tools.
    mkdir -p -- "$install_bin_dir"
    install_bin_dir=$(cd -- "$install_bin_dir" && pwd -P)
    install_target=$install_bin_dir/$install_name
    [ ! -d "$install_target" ] || fail 'installation target is a directory'
    chmod 755 "$install_tmp/sshore"
    [ "$("$install_tmp/sshore" --version)" = "sshore $install_version" ] || fail 'binary version does not match release'
    # curl | sh uses stdin for the script. Read approval from the terminal
    # when present; an SSH exec channel supplies approval on stdin instead.
    if ( : 2>/dev/null </dev/tty ); then
        exec </dev/tty
    fi
    # The published 0.7.9 executable predates the install command.
    if [ "$install_version" = 0.7.9 ] && ! "$install_tmp/sshore" install --help >/dev/null 2>&1; then
        "$install_tmp/sshore" setup --apply --path "$install_target" || fail 'installation stopped'
    else
        "$install_tmp/sshore" install --apply "here:$install_target" || fail 'installation stopped'
    fi
    printf 'Installed sshore %s: %s\n' "$install_version" "$install_target"
    printf '%s\n' 'SHA256 verified. No sudo or shell configuration changes.'
    printf '%s\n' 'Use matching client/server versions. Remote accounts are updated separately.'
    case ":$PATH:" in
        *":$install_bin_dir:"*) ;;
        *) printf 'Directory is not on PATH. Use the full path, or add %s to PATH.\n' "$install_bin_dir" ;;
    esac
}

valid_version() {
    # No shell expressions, URL path components, prereleases or ambiguous names.
    printf '%s\n' "$1" | awk '/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ {ok=1} END {exit !(ok && NR==1)}'
}
verify_archive() {
    [ -f "$1" ] && [ -f "$2" ] &&
        [ "$(wc -c < "$1")" -le 262144 ] &&
        [ "$(wc -c < "$2")" -le 67108864 ] || return 1
    expected=$(awk -v name="$install_asset" '$2==name || $2=="*" name { hash=$1; n++ } END { if (n==1) print tolower(hash); else exit 1 }' "$1") || return 1
    if [ "$install_hasher" = sha256sum ]; then
        actual=$(sha256sum "$2") || return 1
    else
        actual=$(shasum -a 256 "$2") || return 1
    fi
    [ "${actual%% *}" = "$expected" ]
}
download() {
    curl --disable --max-filesize "$3" --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --tlsv1.2 --connect-timeout 15 --max-time 180 --retry 2 "$1" -o "$2" || fail "download failed: $1"
}

# Wrapping execution in a function prevents a partial download from running an
# incomplete installer body. Update approval uses the controlling terminal.
main "$@"
