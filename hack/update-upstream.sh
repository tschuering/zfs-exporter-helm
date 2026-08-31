#!/usr/bin/env bash
# Move the pinned zfs_exporter release to another version.
#
#   hack/update-upstream.sh 2.4.2
#
# The script downloads the archive for each supported architecture and hashes
# it locally. It then rewrites checksums.txt and the default of the Dockerfile
# ARG ZFS_EXPORTER_VERSION. Each digest comes from the bytes that this script
# fetched, and not from the release's own sha256sums.txt. Thus the diff
# shows what this script read. Compare it with upstream before you merge.
set -euo pipefail

readonly ARCHES=(amd64 arm64)
readonly REPO=pdf/zfs_exporter

# macOS has no sha256sum. shasum is the native tool there.
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

version="${1:-}"
if [[ ! ${version} =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "usage: ${0##*/} VERSION   (for example 2.4.2, no leading v)" >&2
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
    curl -fsSL --retry 3 --retry-all-errors -o "${workdir}/${archive}" "${url}"
    digest="$(sha256 "${workdir}/${archive}")"
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
echo "The chart's own version comes from the git tag. This script leaves it alone."
