#!/bin/sh
# Stage the given binaries and every shared object ldd resolves for them into
# a rootfs tree, preserving absolute paths, so the result can be COPYed onto a
# distroless base as-is.
#
#   collect-runtime-deps.sh /rootfs /usr/sbin/zpool /usr/sbin/zfs
#
# Only direct and transitive NEEDED entries are covered. Anything a binary
# dlopen()s at run time is invisible here -- if a future OpenZFS grows one,
# the failure shows up as a missing-library error at exporter start, not at
# build time.
set -eu

dest="${1:?usage: collect-runtime-deps.sh DEST BINARY...}"
shift

mkdir -p "${dest}"

for bin in "$@"; do
    test -x "${bin}" || { echo "not executable: ${bin}" >&2; exit 1; }
    install -D "${bin}" "${dest}${bin}"

    ldd "${bin}" | awk '{ for (i = 1; i <= NF; i++) if (substr($i, 1, 1) == "/") print $i }' \
    | sort -u \
    | while read -r lib; do
        test -e "${dest}${lib}" || install -D "${lib}" "${dest}${lib}"
    done
done

echo "staged into ${dest}:"
find "${dest}" -type f | sort
