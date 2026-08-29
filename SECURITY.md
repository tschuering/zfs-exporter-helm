# Security

## Reporting

Report vulnerabilities privately through GitHub's "Report a vulnerability"
button on the Security tab. Please do not open a public issue for anything
exploitable.

Include the image digest or chart version, what you observed, and how to
reproduce it. Expect an acknowledgement within a week.

## Scope

This repository packages [pdf/zfs_exporter][upstream]; it does not maintain it.

- **Packaging** — the Dockerfile, the chart, the workflows, the pinned digests:
  report here.
- **The exporter itself** — a crash, a metrics leak, a parsing flaw: report to
  [upstream][] first. Tell us too if the packaging makes it worse.
- **OpenZFS** — report to the [OpenZFS project][openzfs].

[upstream]: https://github.com/pdf/zfs_exporter/security
[openzfs]: https://github.com/openzfs/zfs/security

## What the deployment assumes

The chart runs the exporter as root with the host's `/dev/zfs` mounted, because
that device is `0600 root:root` and the ioctls need DAC ownership. Everything
else is dropped — no capabilities, read-only rootfs, no privilege escalation,
no host namespaces, no service account token.

A consequence worth stating plainly: anyone who can execute in this pod can
read every pool and dataset property on the node — layout, names, capacities.
The metrics endpoint exposes the same, so treat `:9134` as sensitive and keep
it off untrusted networks. `networkPolicy.enabled` in the chart restricts who
may reach it.

## Known findings in the exporter binary

This image ships the bytes upstream released, unmodified. Upstream currently
builds with an older Go toolchain, so a scan of `/usr/local/bin/zfs_exporter`
reports Go stdlib and `golang.org/x/{crypto,net,text}` advisories -- at the
time of writing, 38 rated HIGH or CRITICAL, 23 of them stdlib.

They are accepted rather than fixed, deliberately. Clearing them would mean
compiling from source with a newer toolchain and bumping dependencies, which
would make this image something other than a packaging of the upstream
release -- and that provenance is the reason to use it. Practically, the
exposure is limited: the exporter parses the output of two local commands and
serves a metrics endpoint that should not face untrusted input.

CI reflects the split. Findings in the OS layer block the build, because the
base image and the two packages on top of it are this repository's decision.
Findings in the binary are uploaded to the Security tab and reviewed there.

If upstream cuts a release built on a current toolchain, bumping the pin
through `hack/update-upstream.sh` is the fix.

## Supply chain

- Base images pinned by digest, updated by Renovate.
- The upstream binary is verified against per-architecture digests committed in
  `checksums.txt`, computed by downloading and hashing rather than copied from
  the release's own sums file.
- GitHub Actions pinned to commit SHAs, not tags.
- Releases are cosign-signed keyless, with an SPDX SBOM and build provenance
  attached as attestations.
