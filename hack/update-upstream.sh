#!/usr/bin/env bash
# Move the pinned zfs_exporter release to another version.
#
#   hack/update-upstream.sh 2.4.2
#
# Downloads each supported architecture's archive, hashes it here, rewrites
# checksums.txt and the Dockerfile's ZFS_EXPORTER_VERSION default. The digests
# come from the bytes actually fetched rather than from the release's own
# sha256sums.txt, so what lands in the diff is something this script saw --
# review it against upstream before merging.
set -Eeuo pipefail

readonly ARCHES=(amd64 arm64)
readonly REPO=pdf/zfs_exporter

version="${1:-}"
if [[ -z ${version} ]]; then
    echo "usage: ${0##*/} VERSION   (e.g. 2.4.2, no leading v)" >&2
    exit 64
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

lines=()
for arch in "${ARCHES[@]}"; do
    archive="zfs_exporter-${version}.linux-${arch}.tar.gz"
    url="https://github.com/${REPO}/releases/download/v${version}/${archive}"
    echo "fetching ${url}" >&2
    curl -fsSL --retry 3 -o "${workdir}/${archive}" "${url}"
    digest="$(sha256sum "${workdir}/${archive}" | cut -d' ' -f1)"
    lines+=("${digest}  ${archive}")
done

# Keep the header, replace the digest lines.
{
    sed '/^[^#]/d' "${root}/checksums.txt"
    printf '%s\n' "${lines[@]}"
} > "${workdir}/checksums.txt"
mv "${workdir}/checksums.txt" "${root}/checksums.txt"

sed -i.bak -E "s/^(ARG ZFS_EXPORTER_VERSION=).*/\1${version}/" "${root}/Dockerfile"
rm -f "${root}/Dockerfile.bak"

sed -i.bak -E "s/^(appVersion: ).*/\1\"${version}\"/" "${root}/charts/zfs-exporter/Chart.yaml"
rm -f "${root}/charts/zfs-exporter/Chart.yaml.bak"

echo
echo "pinned zfs_exporter ${version}:"
printf '  %s\n' "${lines[@]}"
echo
echo "Chart.yaml appVersion and the Dockerfile ARG were updated too."
echo "Bump the chart's own version by hand -- it is not the exporter's."
