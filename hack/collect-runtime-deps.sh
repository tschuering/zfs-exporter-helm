#!/bin/sh
# Stage a self-contained OpenZFS userland tree: the given binaries into
# DEST/sbin, and every shared object ldd resolves for them -- the dynamic
# loader included -- flattened into DEST/lib.
#
#   collect-runtime-deps.sh /rootfs/opt/zfs/2.3 /usr/sbin/zpool /usr/sbin/zfs
#
# Flat rather than path-preserving because the image carries more than one of
# these trees, each built against its own glibc. Nothing is installed at a
# standard path, so nothing collides; cmd/zfs-shim runs a tree by invoking its
# own loader with --library-path, which is what keeps the two apart.
#
# Only direct and transitive NEEDED entries are covered. Anything a binary
# dlopen()s at run time is invisible here -- if a future OpenZFS grows one, it
# surfaces as a missing-library error when the exporter first shells out.
set -eu

dest="${1:?usage: collect-runtime-deps.sh DEST BINARY...}"
shift

mkdir -p "${dest}/sbin" "${dest}/lib"

for bin in "$@"; do
    test -x "${bin}" || { echo "not executable: ${bin}" >&2; exit 1; }
    install -m 0755 "${bin}" "${dest}/sbin/$(basename "${bin}")"

    ldd "${bin}" | awk '{ for (i = 1; i <= NF; i++) if (substr($i, 1, 1) == "/") print $i }' \
    | sort -u \
    | while read -r lib; do
        test -e "${dest}/lib/$(basename "${lib}")" \
            || install -m 0755 "${lib}" "${dest}/lib/$(basename "${lib}")"
    done
done

# The loader has to be there or the tree cannot be started at all.
if ! ls "${dest}"/lib/ld-*.so* >/dev/null 2>&1; then
    echo "no dynamic loader staged into ${dest}/lib" >&2
    exit 1
fi

echo "staged ${dest}:"
find "${dest}" -type f | sort | sed 's/^/  /'
