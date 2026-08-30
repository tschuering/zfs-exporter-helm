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

The exporter runs unprivileged. It drops every capability, it uses a
`RuntimeDefault` seccomp profile, it has a read-only root filesystem, and it
has no host mounts. `/dev/zfs` reaches it through the bundled device plugin,
and not through a hostPath. That is what makes those settings real.
`privileged: true` would discard the capability drop and the seccomp profile
completely.

With no capabilities, OpenZFS permits the reads that the exporter performs, and
the kernel denies every other operation. `zfs_secpolicy_read` requires no
privilege. Every mutating ioctl goes through `secpolicy_sys_config` or
`secpolicy_zfs`, and both of those are `CAP_SYS_ADMIN` or an error.

The device plugin *is* privileged, because Kubernetes documents that the
kubelet's plugin directory requires it. The plugin has no network listener. It
speaks only to the kubelet over a Unix socket, and it never opens the device
that it advertises. The node therefore still runs one privileged pod. The
change is that this pod is not the one that serves HTTP.

One consequence deserves a plain statement. Anyone who can reach the metrics
endpoint can read every pool and dataset property on the node: layout, names
and capacities. Treat `:9134` as sensitive and keep it off untrusted networks.
`networkPolicy.enabled` in the chart restricts who can reach it.

## Known findings in the exporter binary

This image ships the bytes that upstream released, unmodified. Upstream
currently builds with an older Go toolchain, so a scan of
`/usr/local/bin/zfs_exporter` reports Go stdlib advisories and
`golang.org/x/{crypto,net,text}` advisories. At the time of writing, 38 of
those are rated HIGH or CRITICAL, and 23 of the 38 are stdlib.

These findings are accepted rather than fixed, and that is deliberate. To clear
them, someone would compile from source with a newer toolchain and raise the
dependency versions. That would make this image something other than a
packaging of the upstream release, and that provenance is the reason to use it.
In practice the exposure is limited. The exporter parses the output of two
local commands, and it serves a metrics endpoint that should not face untrusted
input.

CI follows the same split. A finding in the OS layer blocks the build, because
the base image and the two packages above it are this repository's decision. A
finding in the binary is uploaded to the Security tab and reviewed there.

If upstream publishes a release built on a current toolchain, move the pin with
`hack/update-upstream.sh`. That is the fix.

## Supply chain

- Each base image is pinned by digest, and Renovate updates it.
- The upstream binary is verified against per-architecture digests committed in
  `checksums.txt`. Those digests come from a local download and hash. They are
  not copied from the release's own sums file.
- Each GitHub Action is pinned to a commit SHA, not to a tag.
- Each release is cosign-signed keylessly, with an SPDX SBOM and build
  provenance attached as attestations.
