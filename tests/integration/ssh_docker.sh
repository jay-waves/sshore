#!/bin/sh
# Build with the shared Go toolchain and run the SSH fixture in a Linux container.
set -eu

cd "$(dirname "$0")/../.."
if [ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" != linux/amd64 ]; then
    echo 'The local Docker test runner requires a Linux amd64 Go toolchain.' >&2
    exit 1
fi
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
GOOS=linux GOARCH=amd64 go build -buildvcs=false -o "$fixture/sshore" .
cp tests/integration/ssh_e2e.py "$fixture/"

image=sshore-ssh-test:local
docker build --platform linux/amd64 -t "$image" - <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl openssh-server openssh-sftp-server python3 procps \
    && useradd --create-home --shell /bin/sh --password "*" sshore-fixture \
    && mkdir -p /run/sshd \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE

# Each scenario gets fresh account daemon state and an isolated loopback network.
run_scenario() {
    docker run --rm --init --platform linux/amd64 --network none \
        --mount "type=bind,source=$fixture,target=/fixture,readonly" \
        "$image" sh -ec '
            tr -d "-" </proc/sys/kernel/random/uuid >/etc/machine-id
            ln -s /fixture/sshore /usr/local/bin/sshore
            exec python3 /fixture/ssh_e2e.py --binary /fixture/sshore "$@"
        ' ssh-fixture "$@"
}

if [ "$#" -gt 0 ]; then
    run_scenario "$@"
else
    status=0
    for scenario in core spawn identity nested handoff setup mcp; do
        echo "=== SSH scenario: $scenario ==="
        run_scenario "--$scenario-only" || status=1
    done
    exit "$status"
fi
