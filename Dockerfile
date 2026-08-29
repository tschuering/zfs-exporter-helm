# syntax=docker/dockerfile:1

# zfs_exporter shells out to zpool(8) and zfs(8) -- it does not read
# /proc/spl/kstat the way node_exporter's ZFS collector does. So the image has
# to carry the OpenZFS userland, and that userland talks to the host's kernel
# module over /dev/zfs ioctls. Userland and module must therefore agree on a
# major version: Debian trixie ships OpenZFS 2.3.x, which is what this image
# is built against. Running it against a 2.2 or 2.4 host module is unsupported.
#
# Everything else is kept out. The final stage is distroless -- no shell, no
# package manager, no apt state -- and receives exactly three things: the two
# ZFS binaries, the shared libraries ldd says they need, and the exporter.

ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian13@sha256:9ef50bca108839d5986e4d84b7f7b2d79024c9293b7c35b162c6c55485bd5868

# ---- OpenZFS userland, reduced to what the two binaries actually load -------
FROM ${DEBIAN_IMAGE} AS zfs

# OpenZFS sits in contrib, not main -- Debian keeps it out of main over the
# CDDL/GPL incompatibility, and the slim image enables only main.
#
# DL3008: the package version is deliberately not pinned. What fixes the
# OpenZFS major version here is the digest-pinned base image; pinning
# zfsutils-linux on top of it would only make the build fail the moment
# Debian ships a security update, since the archive drops superseded versions.
# hadolint ignore=DL3008
RUN set -eu; \
    sed -i 's/^Components:.*/Components: main contrib/' \
        /etc/apt/sources.list.d/debian.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends zfsutils-linux; \
    rm -rf /var/lib/apt/lists/*

COPY hack/collect-runtime-deps.sh /usr/local/bin/collect-runtime-deps.sh
RUN /usr/local/bin/collect-runtime-deps.sh /rootfs /usr/sbin/zpool /usr/sbin/zfs

# ---- Upstream release, verified against the digests pinned in this repo -----
FROM ${DEBIAN_IMAGE} AS fetch

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

COPY --from=zfs /rootfs/ /
COPY --from=fetch /out/zfs_exporter /usr/local/bin/zfs_exporter

# zfs_exporter calls exec.Command("zpool", ...) and exec.Command("zfs", ...),
# so both have to resolve on PATH. distroless does not put /usr/sbin on it.
ENV PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin

# Root, and no capabilities beyond it: /dev/zfs is mode 0600 root:root, so the
# ioctls need DAC ownership rather than any capability. The chart drops the
# whole bounding set and mounts a read-only rootfs on top of this. DL3002 is
# the rule that says a final USER should not be root; here it is the point.
# hadolint ignore=DL3002
USER 0:0

EXPOSE 9134

ENTRYPOINT ["/usr/local/bin/zfs_exporter"]
CMD ["--web.listen-address=:9134"]
