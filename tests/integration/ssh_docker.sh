#!/bin/sh
# Run the Go SSH integration test against OpenSSH in a disposable Linux container.
set -eu
cd "$(dirname "$0")/../.."
if [ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" != linux/amd64 ]; then
    echo 'The Docker fixture requires a Linux amd64 Go toolchain.' >&2
    exit 1
fi
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
chmod 755 "$fixture"
go run ./scripts go build -buildvcs=false -ldflags '-X main.version=0.0.0' -o "$fixture/sshore" .
go test -c -o "$fixture/integration.test" ./tests/integration

image=sshore-ssh-test:local
docker build --platform linux/amd64 -t "$image" - <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-server procps \
    && useradd --create-home --shell /bin/sh --password '*' sshore-fixture \
    && mkdir -p /run/sshd
DOCKERFILE

docker run --rm --init --platform linux/amd64 --network none \
    --mount "type=bind,source=$fixture,target=/fixture,readonly" \
    "$image" sh -ec '
        ssh-keygen -q -t ed25519 -N "" -f /tmp/fixture-key
        ssh-keygen -q -t ed25519 -N "" -f /tmp/fixture-host
        install -o sshore-fixture -g sshore-fixture -m 600 /tmp/fixture-key.pub /home/sshore-fixture/authorized_keys
        printf "[127.0.0.1]:2222 " > /tmp/fixture-known-hosts
        cat /tmp/fixture-host.pub >> /tmp/fixture-known-hosts
        /usr/sbin/sshd -p 2222 -h /tmp/fixture-host -o ListenAddress=127.0.0.1 \
            -o AuthorizedKeysFile=authorized_keys -o PasswordAuthentication=no -o UsePAM=yes
        ln -s /fixture/sshore /usr/local/bin/sshore
        export SSHORE_TEST_OPENSSH=/fixture/sshore TERM=xterm NO_COLOR=1
        exec /fixture/integration.test -test.run "^TestOpenSSH$" -test.v
    '
