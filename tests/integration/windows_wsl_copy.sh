#!/bin/sh
set -eu

tmp=$(mktemp -d)
release_server=
trap 'if [ -n "$release_server" ]; then kill "$release_server" 2>/dev/null || true; wait "$release_server" || true; fi; rm -rf "$tmp"' EXIT
cp "$1" "$tmp/sshore"
cp "$2" "$tmp/key"
cp "$3" "$tmp/hosts"
cp "$4" "$tmp/release-server"
chmod 700 "$tmp/release-server"
"$tmp/release-server" "$5" &
release_server=$!
chmod 700 "$tmp/sshore"
chmod 600 "$tmp/key"
cd "$tmp"
tries=0
until wget -q -O /dev/null http://127.0.0.1:22221/v0.0.0/SHA256SUMS; do
    tries=$((tries + 1))
    if [ "$tries" -ge 10 ]; then
        echo 'Release fixture did not start' >&2
        exit 1
    fi
    sleep 1
done

copy() {
    ./sshore cp "$@" --ssh -F none -p 22222 -l sshore-fixture -i "$tmp/key" \
        -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$tmp/hosts"
}

printf 'WSL to Windows\000\377\r\n' > original
copy original '127.0.0.1:~/WSL transfer.bin'
copy '127.0.0.1:C:/Users/sshore-fixture/WSL transfer.bin' downloaded
cmp original downloaded

printf 'replacement\n' > replacement
if copy replacement '127.0.0.1:~/WSL transfer.bin' > overwrite.log 2>&1; then
    echo 'Windows cp-agent overwrote an existing file' >&2
    exit 1
fi
cat overwrite.log
grep -F 'destination already exists' overwrite.log
copy '127.0.0.1:~/WSL transfer.bin' after-refusal
cmp original after-refusal

echo 'Windows OpenSSH copy from WSL passed'
