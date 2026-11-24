#!/bin/sh
# PID 1 in the disposable Alpine VM.
set -eu
mkdir -p /dev /proc /sys
mount -t devtmpfs devtmpfs /dev
mkdir /dev/pts
mount -t devpts -o ptmxmode=0666 devpts /dev/pts
ln -sf pts/ptmx /dev/ptmx
mount -t proc proc /proc
mount -t sysfs sysfs /sys
ip link set lo up
modprobe fuse

status=0
/bin/sh /usr/local/bin/alpine-fixture || status=$?
printf '\nSSHORE_ALPINE_TEST_STATUS=%d\n' "$status"
poweroff -f
