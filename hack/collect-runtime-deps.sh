#!/bin/sh
# Stage a self-contained OpenZFS userland tree. The script copies the given
# binaries into DEST/sbin. It also copies every shared object that ldd resolves
# for them, and the dynamic loader, into a flat DEST/lib.
#
#   collect-runtime-deps.sh /rootfs/opt/zfs/2.3 /usr/sbin/zpool /usr/sbin/zfs
#
# The layout is flat and does not keep the source paths, because the image
# carries more than one of these trees, and each tree is built against its own
# glibc. No tree is installed at a standard path, so no file collides.
# cmd/zfs-shim runs a tree through that tree's own loader with --library-path,
# which is what keeps the trees apart.
#
# This covers only direct and transitive NEEDED entries. A library that a
# binary opens with dlopen() at run time is not visible here. If a future
# OpenZFS adds one, it appears as a missing-library error the first time the
# exporter runs zpool or zfs.
set -eu

dest="${1:?usage: collect-runtime-deps.sh DEST BINARY...}"
shift

mkdir -p "${dest}/sbin" "${dest}/lib"

for bin in "$@"; do
    test -x "${bin}" || { echo "not executable: ${bin}" >&2; exit 1; }
    install -m 0755 "${bin}" "${dest}/sbin/$(basename "${bin}")"

    # ldd runs at the head of a pipeline, and this shell has no pipefail. A
    # capture into a variable first is what makes an ldd failure stop the
    # script.
    deps="$(ldd "${bin}")" || { echo "ldd failed for ${bin}" >&2; exit 1; }
    printf '%s\n' "${deps}" \
    | awk '{ for (i = 1; i <= NF; i++) if (substr($i, 1, 1) == "/") print $i }' \
    | sort -u \
    | while read -r lib; do
        test -e "${dest}/lib/$(basename "${lib}")" \
            || install -m 0755 "${lib}" "${dest}/lib/$(basename "${lib}")"
    done
done

# The loader must be present, or nothing can start the tree.
if ! ls "${dest}"/lib/ld-*.so* >/dev/null 2>&1; then
    echo "no dynamic loader staged into ${dest}/lib" >&2
    exit 1
fi

echo "staged ${dest}:"
find "${dest}" -type f | sort | sed 's/^/  /'
