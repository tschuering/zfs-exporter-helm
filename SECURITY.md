# Security

## Reporting

Report a vulnerability privately. Use the "Report a vulnerability" button on
GitHub's Security tab. Please do not open a public issue for anything
exploitable.

If you cannot use that form, mail tobias@schuering.xyz instead.

Include the image digest or the chart version, what you observed, and the steps
to reproduce it. Expect an acknowledgement within a week.

## Scope

This repository packages [pdf/zfs_exporter][upstream]. It does not maintain
the exporter.

- **Packaging** — the Dockerfile, the chart, the workflows, the pinned digests.
  Report these here.
- **The exporter itself** — a crash, a metrics leak, a parsing flaw. Report
  these to [upstream][] first. Tell us as well if the packaging makes the
  problem worse.
- **OpenZFS** — report to the [OpenZFS project][openzfs].

[upstream]: https://github.com/pdf/zfs_exporter/security
[openzfs]: https://github.com/openzfs/zfs/security

## What the deployment assumes

**No pod in this chart sets `privileged: true`.** Both pods hold an empty
capability set, use a `RuntimeDefault` seccomp profile, have a read-only root
filesystem, and share no host namespace. Measured in running containers:

| | uid | `CapEff` | seccomp |
| --- | --- | --- | --- |
| exporter | 65532 | `0000000000000000` | filter active |
| device plugin | 0 | `0000000000000000` | filter active |

The exporter runs as a non-root uid and meets the **restricted** Pod Security
Standard. It has no host mounts. `/dev/zfs` reaches it through the bundled
device plugin, and not through a hostPath.

The plugin runs as uid 0, and that is its one requirement. The kubelet plugin
directory is mode `0755 root:root`, so only uid 0 can create the socket there.
It needs no capability for that, and no capability for the `stat()` that it
performs on the device. It has no network listener, it speaks only to the
kubelet over a Unix socket, and it never opens the device that it advertises.
It mounts that device `readOnly`, because a `stat()` needs no more.

The exporter receives the device with `rw` in its cgroup rule, and that is not
a weakness. libzfs opens `/dev/zfs` with `O_RDWR` even to list a pool, so a
read-only rule fails at `open()`. What refuses a write to a pool is the
`CAP_SYS_ADMIN` check inside each mutating ioctl, and not the mode of the
device.

Most of this is not configurable. `runAsUser` and `runAsGroup` are the only
security values the chart takes. It renders `privileged: false`,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`,
`capabilities.drop: [ALL]`, the seccomp profile, and `hostNetwork: false`,
`hostPID: false` and `hostIPC: false` for both pods, and it has no key that
changes any of them. A pod cannot be made privileged through this chart: the
setting would discard the guarantees on this page and give nothing back,
because the device still comes from the plugin.

With no capabilities, OpenZFS permits the reads that the exporter performs, and
the kernel denies every other operation. `zfs_secpolicy_read` requires no
privilege. Every mutating ioctl goes through `secpolicy_sys_config` or
`secpolicy_zfs`, and both of those are `CAP_SYS_ADMIN` or an error.

One consequence deserves a plain statement. Anyone who can reach the metrics
endpoint can read every pool and dataset property on the node: layout, names
and capacities. Treat `:9134` as sensitive and keep it off untrusted networks.
`networkPolicy.enabled` in the chart restricts who can reach it.

### Why the exporter needs no root

Two separate rules guard a device node. The device cgroup decides whether the
container can open it at all, and that rule ignores the uid. The device plugin
is what satisfies it. The file permissions decide which uid can open the node,
and OpenZFS ships `90-zfs.rules`, which sets `/dev/zfs` to mode `0666`.

That rule also carries `OPTIONS+="static_node=zfs"`. On a host where udev never
ran, the kernel creates `/dev/zfs` at mode `0600 root:root` and nothing relaxes
it. The exporter then stops at `open()` with `permission denied`. Run
`ls -l /dev/zfs` on the node. If it reads `crw-------`, set the `runAsUser` and
`runAsGroup` values to 0. `runAsNonRoot` follows them, and every other setting
stays as it is.

### One namespace consequence

The plugin mounts the kubelet plugin directory as a hostPath, and the
**baseline** Pod Security Standard forbids hostPath volumes. The release
namespace therefore needs
`pod-security.kubernetes.io/enforce: privileged`. That level applies to the
exporter as well, although the exporter satisfies **restricted**. Install the
plugin in its own namespace if the exporter's namespace must enforce a level.

## Known findings in the exporter binary

This image ships the bytes that upstream released, unmodified. Upstream builds
with Go 1.24.2, so a scan of `/usr/local/bin/zfs_exporter` reports Go stdlib
advisories and `golang.org/x/{crypto,net,text}` advisories.

A scan on 2026-08-31 reported 75 open alerts. Every one of them sits in that
single file. The OS layer and this repository's own binaries produce none:

| Severity | `stdlib` | `golang.org/x/*` | Total |
| --- | --- | --- | --- |
| CRITICAL | 1 | 0 | 1 |
| HIGH | 22 | 15 | 37 |
| MEDIUM | 26 | 7 | 33 |
| LOW and below | 2 | 2 | 4 |

The [Security tab][alerts] holds the current count.

Reachability matters more than the count, and `govulncheck` measures it. It
reported 33 of the 75 as reachable, and all 33 come from the Go 1.24.2 standard
library. About half of the 33 sit behind `--web.config.file`. They are dead
code unless you enable TLS. The rest sit on the normal scrape path:
`os/exec.LookPath`, `html/template` for the landing page, and the `net/url` and
`net/http` request handling.

No advisory in a `golang.org/x/` module is reachable. Those 24 alerts are noise
for anyone who repackages the release.

These findings are accepted here, and not fixed here. To clear them, someone
compiles from source with a newer toolchain and raises the dependency versions.
That makes this image something other than a packaging of the upstream release,
and that provenance is the reason to use it.

CI follows the same split. A finding in the OS layer blocks the build, because
the base image and the two packages above it are this repository's decision. CI
uploads a finding in the binary to the Security tab, and we review it there.

### The fix belongs upstream

Go 1.24 is out of support. Only the 1.26 and 1.27 lines still get fixes. Thus
the real remedy is a new upstream release built on a supported toolchain.

[pdf/zfs_exporter#71][upstream-issue] asks upstream for that move. The change
is prepared and tested against Go 1.26.7. It moves version strings only, it
changes no source file, and it takes the alert count to zero. Upstream
CONTRIBUTING asks for an issue before a PR, so the issue came first.

The change has one consequence for a reader of this repository. The
`go.mod` language version must move from 1.24 to 1.26, because the newer
`golang.org/x/` modules require 1.25 or later. That opts the binary into the
newer GODEBUG defaults. Two of them matter here: `containermaxprocs` makes
GOMAXPROCS follow the cgroup CPU limit, and `tlssha1` rejects SHA-1 signatures
in TLS.

If upstream publishes a release built on a supported toolchain, move the pin
with `hack/update-upstream.sh`. That is the fix.

[upstream-issue]: https://github.com/pdf/zfs_exporter/issues/71
[alerts]: https://github.com/tschuering/zfs-exporter-helm/security/code-scanning

## Supply chain

- Each base image is pinned by digest, and Renovate updates it.
- The upstream binary is verified against per-architecture digests committed in
  `checksums.txt`. Those digests come from a local download and hash. They are
  not copied from the release's own sums file.
- Each GitHub Action is pinned to a commit SHA, not to a tag.
- Each release is cosign-signed keylessly, with an SPDX SBOM and build
  provenance attached as attestations.
