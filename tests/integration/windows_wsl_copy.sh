#!/bin/sh
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp "$1" "$tmp/sshore"
cp "$2" "$tmp/key"
cp "$3" "$tmp/hosts"
chmod 700 "$tmp/sshore"
chmod 600 "$tmp/key"
cd "$tmp"

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
