#!/bin/sh
# Run as root in the disposable CI WSL distribution.
set -eu
artifacts=$1
windows_dir=$2
fixture=$3

useradd --create-home --shell /bin/sh --password '*' sshore-fixture
chmod 755 "$fixture"
install -m 755 "$artifacts/sshore" "$artifacts/sshore-other" "$artifacts/integration.test" "$fixture/"
ln -s "$fixture/sshore" /usr/local/bin/sshore
install -d -m 755 /run/sshd
install -d -o sshore-fixture -g sshore-fixture /home/sshore-fixture/share

ssh-keygen -q -t ed25519 -N '' -f "$fixture/identity"
ssh-keygen -q -t ed25519 -N '' -f "$fixture/host"
install -o sshore-fixture -g sshore-fixture -m 600 "$fixture/identity.pub" /home/sshore-fixture/authorized_keys
{
    printf '[127.0.0.1]:2222 '
    cat "$fixture/host.pub"
} > "$fixture/known_hosts"
cp "$fixture/identity" "$fixture/known_hosts" "$windows_dir/"

exec /usr/sbin/sshd -D -e -f /dev/null -o 'ListenAddress=127.0.0.1' -p 2222 \
    -h "$fixture/host" -o UsePAM=yes -o AuthorizedKeysFile=authorized_keys \
    -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no \
    -o AllowUsers=sshore-fixture
