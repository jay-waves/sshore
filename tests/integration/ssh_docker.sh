#!/bin/sh
# Run the Go SSH integration test against OpenSSH in a disposable Linux container.
set -eu
if [ "$#" -gt 1 ]; then
    echo 'usage: ssh_docker.sh [GO_TEST_RUN_PATTERN]' >&2
    exit 2
fi
pattern=${1:-^TestOpenSSH$}
cd "$(dirname "$0")/../.."
if [ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" != linux/amd64 ]; then
    echo 'The Docker fixture requires a Linux amd64 Go toolchain.' >&2
    exit 1
fi
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
chmod 755 "$fixture"
go build -o "$fixture/build" ./scripts
"$fixture/build" go build -buildvcs=false -ldflags '-X main.version=0.0.0' -o "$fixture/sshore" .
"$fixture/build" go build -buildvcs=false -ldflags '-X main.version=0.0.1' -o "$fixture/sshore-other" .
go test -tags=integration -c -o "$fixture/integration.test" ./tests/integration

image=sshore-ssh-test:local
docker build --platform linux/amd64 -t "$image" - <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-server procps fuse3 \
    && useradd --create-home --shell /bin/sh --password '*' sshore-fixture \
    && mkdir -p /run/sshd
DOCKERFILE

set --
if [ "${SSHORE_TEST_FUSE:-0}" = 1 ]; then
    set -- --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined
fi
docker run --rm --init --platform linux/amd64 --network none "$@" \
    --env SSHORE_TEST_FUSE \
    --mount "type=bind,source=$fixture,target=/fixture,readonly" \
    "$image" sh -ec '
        ssh-keygen -q -t ed25519 -N "" -f /tmp/fixture-key
        ssh-keygen -q -t ed25519 -N "" -f /tmp/fixture-host
        install -o sshore-fixture -g sshore-fixture -m 600 /tmp/fixture-key.pub /home/sshore-fixture/authorized_keys
        printf "[127.0.0.1]:2222 " > /tmp/fixture-known-hosts
        cat /tmp/fixture-host.pub >> /tmp/fixture-known-hosts
        /usr/sbin/sshd -E /tmp/fixture-sshd.log -p 2222 -h /tmp/fixture-host -o ListenAddress=127.0.0.1 \
            -o AuthorizedKeysFile=authorized_keys -o PasswordAuthentication=no -o UsePAM=yes
        ln -s /fixture/sshore /usr/local/bin/sshore
        export SSHORE_TEST_OPENSSH=/fixture/sshore TERM=xterm NO_COLOR=1
        /fixture/integration.test -test.run "$1" -test.v -test.timeout 75s || {
            status=$?
            cat /tmp/fixture-sshd.log >&2
            exit "$status"
        }
    ' sshore-fixture "$pattern"
