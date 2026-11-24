#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

fixture=$(mktemp -d)
chmod 755 "$fixture"
cp /fixture/sshore /fixture/sshore-other "$fixture/"
mkdir -p /run/sshd
ssh-keygen -q -t ed25519 -N '' -f "$fixture/identity"
ssh-keygen -q -t ed25519 -N '' -f "$fixture/host"
install -o sshore-fixture -g sshore-fixture -m 600 "$fixture/identity.pub" /home/sshore-fixture/authorized_keys
{
    printf '[127.0.0.1]:2222 '
    cat "$fixture/host.pub"
} > "$fixture/known_hosts"
/usr/sbin/sshd -E "$fixture/sshd.log" -f /dev/null -p 2222 -h "$fixture/host" \
    -o ListenAddress=127.0.0.1 -o AuthorizedKeysFile=authorized_keys \
    -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no \
    -o AllowUsers=sshore-fixture
ln -s "$fixture/sshore" /usr/local/bin/sshore

export SSHORE_TEST_OPENSSH="$fixture/sshore" SSHORE_STATE_DIR="$fixture/state" TERM=xterm NO_COLOR=1
cd /fixture/repo/tests/integration
/fixture/integration.test -test.run "$(cat /fixture/pattern)" -test.v -test.timeout 90s || {
    status=$?
    cat "$fixture/sshd.log" >&2
    exit "$status"
}
