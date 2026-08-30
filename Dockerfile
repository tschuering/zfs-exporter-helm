# syntax=docker/dockerfile:1

# zfs_exporter shells out to zpool(8) and zfs(8) -- it does not read
# /proc/spl/kstat the way node_exporter's ZFS collector does. So the image has
# to carry the OpenZFS userland, and that userland talks to the host's kernel
# module over /dev/zfs ioctls.
#
# Rather than pick one version and require the host to match, the image bundles
# both currently supported OpenZFS branches and chooses between them at run
# time. cmd/zfs-shim reads /sys/module/zfs/version -- readable in a container
# with no mount, since module state is not namespaced -- and executes the
# matching tree. A host running either 2.3.x or 2.4.x is served by the same
# tag, and upgrading the host across branches needs no change here.
#
# Each tree is self-contained: its own zpool, zfs, libraries and dynamic
# loader, under /opt/zfs/<major>.<minor>. That is what lets two userlands built
# against different glibc versions live in one image -- neither is installed at
# a standard path, and the shim starts each through its own loader.
#
# The final stage is distroless: no shell, no package manager, no apt state.

ARG DEBIAN_23_IMAGE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
ARG DEBIAN_24_IMAGE=debian:forky-slim@sha256:91b0aaebf7a1ccacfe7a9cbff6ab2d6be7d9b3b6cf1dfcf44b25f9095c0e0464
ARG GO_IMAGE=golang:1.26.7-trixie@sha256:e6e8ff4b72b128bb673613645c5ac415e4f537b2390e77a86ffc40622ab56da8
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian13@sha256:9ef50bca108839d5986e4d84b7f7b2d79024c9293b7c35b162c6c55485bd5868

# ---- OpenZFS 2.3.x userland (Debian trixie) --------------------------------
FROM ${DEBIAN_23_IMAGE} AS zfs23
ARG ZFS_23_EXPECT=2.3
COPY hack/collect-runtime-deps.sh /usr/local/bin/collect-runtime-deps.sh
# OpenZFS sits in contrib, not main -- Debian keeps it out of main over the
# CDDL/GPL incompatibility, and the slim image enables only main.
#
# DL3008: the package version is deliberately not pinned. What fixes the
# OpenZFS branch here is the digest-pinned base image, and the assertion below
# fails the build if that base ever moves to a different branch. Pinning the
# package on top would only break the moment Debian ships a security update,
# since the archive drops superseded versions.
# hadolint ignore=DL3008
RUN set -eu; \
    sed -i 's/^Components:.*/Components: main contrib/' \
        /etc/apt/sources.list.d/debian.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends zfsutils-linux; \
    rm -rf /var/lib/apt/lists/*; \
    version="$(dpkg-query -W -f='${Version}' zfsutils-linux)"; \
    branch="${version%%-*}"; \
    branch="${branch%.*}"; \
    test "${branch}" = "${ZFS_23_EXPECT}" || { \
        echo "expected OpenZFS ${ZFS_23_EXPECT} from this base, got ${version}" >&2; \
        exit 1; \
    }; \
    /usr/local/bin/collect-runtime-deps.sh "/rootfs/opt/zfs/${branch}" \
        /usr/sbin/zpool /usr/sbin/zfs

# ---- OpenZFS 2.4.x userland (Debian forky) ---------------------------------
FROM ${DEBIAN_24_IMAGE} AS zfs24
ARG ZFS_24_EXPECT=2.4
COPY hack/collect-runtime-deps.sh /usr/local/bin/collect-runtime-deps.sh
# hadolint ignore=DL3008
RUN set -eu; \
    sed -i 's/^Components:.*/Components: main contrib/' \
        /etc/apt/sources.list.d/debian.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends zfsutils-linux; \
    rm -rf /var/lib/apt/lists/*; \
    version="$(dpkg-query -W -f='${Version}' zfsutils-linux)"; \
    branch="${version%%-*}"; \
    branch="${branch%.*}"; \
    test "${branch}" = "${ZFS_24_EXPECT}" || { \
        echo "expected OpenZFS ${ZFS_24_EXPECT} from this base, got ${version}" >&2; \
        exit 1; \
    }; \
    /usr/local/bin/collect-runtime-deps.sh "/rootfs/opt/zfs/${branch}" \
        /usr/sbin/zpool /usr/sbin/zfs

# ---- The dispatcher --------------------------------------------------------
FROM ${GO_IMAGE} AS shim
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
# Both binaries ship in one image: one image to build, sign, scan and pin, and
# the device plugin is never a different version from the exporter it feeds.
# Static, so neither cares which glibc is around, and stripped because there is
# nothing to debug in production.
#
# The symlinks are made here: the final stage has no shell to make them in.
RUN set -eu; \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
        -o /out/usr/local/bin/zfs-shim ./cmd/zfs-shim; \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
        -o /out/usr/local/bin/zfs-device-plugin ./cmd/zfs-device-plugin; \
    ln -s zfs-shim /out/usr/local/bin/zpool; \
    ln -s zfs-shim /out/usr/local/bin/zfs

# ---- Upstream release, verified against the digests pinned in this repo -----
FROM ${DEBIAN_23_IMAGE} AS fetch

ARG TARGETARCH
# renovate: datasource=github-releases depName=pdf/zfs_exporter extractVersion=^v(?<version>.+)$
ARG ZFS_EXPORTER_VERSION=2.4.1

# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# checksums.txt is reviewed in a git diff rather than fetched from the release
# it verifies -- a sums file served from the same directory as the artifact
# shares its trust root, so anyone able to replace one can replace the other.
# hack/update-upstream.sh regenerates it.
COPY checksums.txt /tmp/checksums.txt

RUN set -eu; \
    archive="zfs_exporter-${ZFS_EXPORTER_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    expected="$(awk -v a="${archive}" '$2 == a { print $1 }' /tmp/checksums.txt)"; \
    test -n "${expected}" || { echo "no digest for ${archive} in checksums.txt" >&2; exit 1; }; \
    curl -fsSL --retry 3 -o /tmp/zfs_exporter.tar.gz \
        "https://github.com/pdf/zfs_exporter/releases/download/v${ZFS_EXPORTER_VERSION}/${archive}"; \
    actual="$(sha256sum /tmp/zfs_exporter.tar.gz)"; \
    actual="${actual%% *}"; \
    test "${actual}" = "${expected}" || { \
        echo "digest mismatch for ${archive}" >&2; \
        echo "  expected ${expected}" >&2; \
        echo "  actual   ${actual}" >&2; \
        exit 1; \
    }; \
    mkdir -p /out; \
    tar -xzf /tmp/zfs_exporter.tar.gz -C /out --strip-components=1 \
        "zfs_exporter-${ZFS_EXPORTER_VERSION}.linux-${TARGETARCH}/zfs_exporter"; \
    chmod 0755 /out/zfs_exporter

# ---- Runtime ---------------------------------------------------------------
FROM ${RUNTIME_IMAGE}

COPY --from=zfs23 /rootfs/ /
COPY --from=zfs24 /rootfs/ /
COPY --from=shim /out/ /
COPY --from=fetch /out/zfs_exporter /usr/local/bin/zfs_exporter

# zfs_exporter calls exec.Command("zpool", ...) and exec.Command("zfs", ...),
# so both have to resolve on PATH -- to the shim, which is why /usr/local/bin
# comes first.
ENV PATH=/usr/local/bin:/usr/bin:/bin

# Root, and no capabilities beyond it: /dev/zfs is mode 0600 root:root, so the
# ioctls need DAC ownership rather than any capability. The chart drops the
# whole bounding set and mounts a read-only rootfs on top of this. DL3002 is
# the rule that says a final USER should not be root; here it is the point.
# hadolint ignore=DL3002
USER 0:0

EXPOSE 9134

ENTRYPOINT ["/usr/local/bin/zfs_exporter"]
CMD ["--web.listen-address=:9134"]
