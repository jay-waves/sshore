#!/bin/sh
# Run the same Alpine OpenSSH/FUSE fixture in Docker or a KVM guest.
set -eu

target=fixture
if [ "${1:-}" = --kvm ]; then
    target=kvm
    shift
fi
if [ "$#" -gt 1 ]; then
    echo 'usage: ssh_docker.sh [--kvm] [GO_TEST_RUN_PATTERN]' >&2
    exit 2
fi
container=${CONTAINER_ENGINE:-docker}
cd "$(dirname "$0")/../.."
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
payload=$fixture/payload
mkdir -p "$payload/repo/tests/integration"
chmod 755 "$fixture" "$payload"
cp install.sh "$payload/repo/"
cp -R tests/integration/testdata "$payload/repo/tests/integration/"
cp tests/integration/alpine_fixture.sh tests/integration/alpine_init.sh "$fixture/"
printf '%s\n' "${1:-^Test(OpenSSH|LinuxInstaller)$}" > "$payload/pattern"

go build -o "$fixture/build" ./scripts
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
"$fixture/build" go build -ldflags '-X main.version=0.0.0' -o "$payload/sshore" .
"$fixture/build" go build -ldflags '-X main.version=0.0.1' -o "$payload/sshore-other" .
go test -tags=integration -c -o "$payload/integration.test" ./tests/integration

"$container" build --platform linux/amd64 --target "$target" --iidfile "$fixture/image-id" \
    -f tests/integration/alpine.Dockerfile "$fixture"
image=$(cat "$fixture/image-id")
if [ "$target" = fixture ]; then
    "$container" run --rm --init --network none --device /dev/fuse --cap-add SYS_ADMIN \
        --security-opt apparmor=unconfined -v "$payload:/fixture:ro,Z" "$image"
    exit
fi

"$container" run --rm --network none --entrypoint tar "$image" -cf - \
    boot/vmlinuz-virt fixture-initramfs.gz > "$fixture/guest.tar"
tar -xf "$fixture/guest.tar" -C "$fixture"
status=0
timeout 150s qemu-system-x86_64 -accel kvm -cpu host -m 768 -smp 2 -nographic -no-reboot -nic none \
    -kernel "$fixture/boot/vmlinuz-virt" -initrd "$fixture/fixture-initramfs.gz" \
    -append 'console=ttyS0 rdinit=/init quiet panic=1' > "$fixture/guest.log" 2>&1 || status=$?
cat "$fixture/guest.log"
[ "$status" -eq 0 ] && tr -d '\r' < "$fixture/guest.log" | grep -qx 'SSHORE_ALPINE_TEST_STATUS=0'
