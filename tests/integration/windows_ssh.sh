#!/bin/sh
# Run as root in the disposable CI WSL distribution.
set -eu
binary=$1
windows_dir=$2
fixture=$(mktemp -d)

useradd --create-home --shell /bin/sh --password '*' sshore-winfsp
install -m 755 "$binary" /usr/local/bin/sshore
install -d -m 755 /run/sshd
install -d -o sshore-winfsp -g sshore-winfsp /home/sshore-winfsp/share

ssh-keygen -q -t ed25519 -N '' -f "$fixture/client"
ssh-keygen -q -t ed25519 -N '' -f "$fixture/host"
install -o sshore-winfsp -g sshore-winfsp -m 600 "$fixture/client.pub" /home/sshore-winfsp/authorized_keys
cp "$fixture/client" "$windows_dir/identity"
{
    printf '[127.0.0.1]:2222 '
    cat "$fixture/host.pub"
} > "$windows_dir/known_hosts"

exec /usr/sbin/sshd -D -e -f /dev/null -o 'ListenAddress=127.0.0.1' -p 2222 \
    -h "$fixture/host" -o UsePAM=yes -o AuthorizedKeysFile=authorized_keys \
    -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no \
    -o AllowUsers=sshore-winfsp
