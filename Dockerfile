# syntax=docker/dockerfile:1

# zfs_exporter runs zpool(8) and zfs(8). It does not read /proc/spl/kstat,
# which is what node_exporter's ZFS collector does. The image must therefore
# carry the OpenZFS userland, and that userland communicates with the host's
# kernel module through /dev/zfs ioctls.
#
# The image does not select one version and require the host to match it. It
# carries both supported OpenZFS branches and selects one at run time.
# cmd/zfs-shim reads /sys/module/zfs/version, which a container can read with
# no mount, because module state is not namespaced. The shim then executes the
# matching tree. One tag therefore serves a host that runs 2.3.x and a host
# that runs 2.4.x, and an upgrade of a host from one branch to the other needs
# no change here.
#
# Each tree is self-contained. It holds its own zpool, zfs, libraries and
# dynamic loader, under /opt/zfs/<major>.<minor>. That is what lets two
# userlands built against different glibc versions live in one image. Neither
# tree is installed at a standard path, and the shim starts each tree through
# that tree's own loader.
#
# The final stage is distroless. It has no shell, no package manager and no apt
# state.

ARG DEBIAN_23_IMAGE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
ARG DEBIAN_24_IMAGE=debian:forky-slim@sha256:91b0aaebf7a1ccacfe7a9cbff6ab2d6be7d9b3b6cf1dfcf44b25f9095c0e0464
ARG GO_IMAGE=golang:1.26.7-trixie@sha256:e6e8ff4b72b128bb673613645c5ac415e4f537b2390e77a86ffc40622ab56da8
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian13@sha256:9ef50bca108839d5986e4d84b7f7b2d79024c9293b7c35b162c6c55485bd5868

# ---- OpenZFS 2.3.x userland (Debian trixie) --------------------------------
FROM ${DEBIAN_23_IMAGE} AS zfs23
ARG ZFS_23_EXPECT=2.3
COPY hack/collect-runtime-deps.sh /usr/local/bin/collect-runtime-deps.sh
# OpenZFS is in contrib, not in main. Debian keeps it out of main because of
# the CDDL/GPL incompatibility, and the slim image enables only main.
#
# DL3008: the package version is deliberately not pinned. The digest-pinned
# base image is what fixes the OpenZFS branch here, and the check below fails
# the build if that base moves to a different branch. A pinned package version
# would break as soon as Debian publishes a security update, because the
# archive removes superseded versions.
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
# Copy both packages. cmd holds the two binaries, and internal holds the
# logging format that they share. A copy of cmd alone builds nothing: the
# import fails at compile time, which is a slow way to find the problem.
COPY cmd ./cmd
COPY internal ./internal
# Both binaries go into one image. That gives one image to build, sign, scan
# and pin, and the device plugin is never a different version from the exporter
# that it serves. The binaries are static, so neither one depends on the glibc
# that is present. They are stripped, because there is nothing to debug in
# production.
#
# The symlinks are made here, because the final stage has no shell to make them
# in.
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

# checksums.txt is reviewed in a git diff. It is not fetched from the release
# that it verifies. A sums file served from the same directory as the artifact
# shares the trust root of that artifact, so anyone who can replace one file
# can replace the other. hack/update-upstream.sh rewrites this file.
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
# so both names must resolve on PATH, and they must resolve to the shim.
# /usr/local/bin is therefore first.
ENV PATH=/usr/local/bin:/usr/bin:/bin

# The user is root, and it holds no capability above that. /dev/zfs is mode
# 0600 root:root, so the ioctls need DAC ownership and no capability. The chart
# drops the whole bounding set and mounts a read-only rootfs above this. DL3002
# is the rule that a final USER should not be root. Here root is the
# intention.
# hadolint ignore=DL3002
USER 0:0

EXPOSE 9134

ENTRYPOINT ["/usr/local/bin/zfs_exporter"]
CMD ["--web.listen-address=:9134"]
