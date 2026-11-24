FROM alpine:3.24 AS fixture
RUN apk add --no-cache openssh-server procps fuse3 && \
    adduser -D -s /bin/sh sshore-fixture && \
    printf 'sshore-fixture:NP\n' | chpasswd -e
COPY alpine_fixture.sh /usr/local/bin/alpine-fixture
ENTRYPOINT ["/bin/sh", "/usr/local/bin/alpine-fixture"]

FROM fixture AS kvm
RUN apk add --no-cache linux-virt cpio
COPY payload /fixture
COPY alpine_init.sh /init
RUN set -o pipefail && chmod +x /init && \
    find bin etc fixture home init lib root run sbin tmp usr var | \
        cpio -o -H newc | gzip -1 > /fixture-initramfs.gz
