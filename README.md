# zfs-exporter

A container image and Helm chart for [pdf/zfs_exporter][upstream] — per-dataset
and per-pool OpenZFS metrics for Prometheus.

Upstream publishes release binaries but no image and no chart. This repository
packages them: a distroless image carrying only the exporter and the OpenZFS
userland it shells out to, and a DaemonSet chart that gives it the one piece of
host access it needs.

[upstream]: https://github.com/pdf/zfs_exporter

## Motivation

I wanted to run this exporter the way I already run node_exporter: as a
DaemonSet, deployed from the same place as everything else, and to keep the
Ubuntu host underneath as thin as I could.

Everything else on my node had already moved into Kubernetes. The ZFS exporter
was the one piece of monitoring still installed on the host — a binary in
`/usr/local/bin`, a systemd unit, a version pin and a checksum, all carried by
configuration management, all needing their own review and their own upgrade
path. One exporter's worth of host state, kept alive for one exporter.

That is a poor trade. A host that runs a kernel, ZFS and a kubelet is a host I
can rebuild without thinking. Every package and unit added on top is something
to patch, something that drifts, and something that has to be reproduced the
next time the machine is reinstalled. Moving the exporter into the cluster puts
it under the same deployment, rollback and upgrade story as every other
workload, and takes the last service off the host.

It stayed on the host longer than everything else for a concrete reason:
upstream publishes no image and no chart, and unlike node_exporter this
exporter needs the OpenZFS userland present next to it. That is the gap this
repository fills.

## Why an image is not trivial

`zfs_exporter` does not read `/proc/spl/kstat` the way node_exporter's ZFS
collector does. It runs `zpool(8)` and `zfs(8)` and parses their output:

```go
exec.Command(`zpool`, `get`, `-Hpo`, `name,property,value`, ...)
exec.Command(`zfs`,   `get`, `-Hprt`, ...)
```

So the image has to carry the OpenZFS userland, and that userland talks to the
**host's** kernel module through `/dev/zfs`.

## Host compatibility

**OpenZFS 2.3.x and 2.4.x, from the same tag.** The image bundles a userland
for each branch and chooses at run time, so a fleet running both is served by
one DaemonSet and upgrading a node across branches needs no change here.

`cmd/zfs-shim` is installed on `PATH` as `zpool` and `zfs` — the names the
exporter execs. It reads the node's branch from `/sys/module/zfs/version`,
which a container can read with no mount at all because module state is not
namespaced, and starts the matching tree:

```
/opt/zfs/2.4/lib/ld-linux-x86-64.so.2 \
    --library-path /opt/zfs/2.4/lib /opt/zfs/2.4/sbin/zpool ...
```

Each tree carries its own libraries **and its own dynamic loader**, which is
what lets two userlands built against different glibc coexist — 2.3 links
`libzfs.so.6`, 2.4 links `libzfs.so.7`, and neither is installed at a standard
path. Nothing is guessed: a node on a branch the image does not carry gets

```
zfs-shim: no bundled OpenZFS userland for host version 2.5; this image carries 2.3, 2.4
```

rather than a confusing ioctl failure. `zfsUserlandVersion` in the chart
overrides the detection.

Adding a branch is a builder stage and a base image — see the `zfs23` and
`zfs24` stages in the [Dockerfile](Dockerfile). Each stage asserts the branch
the base actually shipped, so a base image that moves to a new OpenZFS fails
the build instead of quietly changing what the image supports.

## What is in the image

On `gcr.io/distroless/base-debian13` — no shell, no package manager, no apt
state:

| Path | Source |
| --- | --- |
| `/usr/local/bin/zfs_exporter` | upstream release, sha256 pinned in `checksums.txt` |
| `/usr/local/bin/{zpool,zfs}` | symlinks to `zfs-shim`, built from `cmd/zfs-shim` |
| `/opt/zfs/2.3/` | Debian trixie `zfsutils-linux`, with its libraries and loader |
| `/opt/zfs/2.4/` | Debian forky `zfsutils-linux`, likewise |

Around 84 MB. Both `linux/amd64` and `linux/arm64`.

The exporter binary is verified against digests kept in this repository rather
than the `sha256sums.txt` served next to the release. A sums file in the same
directory as the artifact shares its trust root — anyone able to replace one
can replace the other — so pinning it here is what makes the digest reviewable
in a diff. `hack/update-upstream.sh` regenerates both by downloading and
hashing each archive.

## Security posture

The pod runs as root, and that is the entire privilege it holds. `/dev/zfs` is
mode `0600 root:root`, so the ioctls behind `zpool list` need DAC ownership —
not a capability, not `privileged: true`.

Everything else is off:

- every capability dropped (`capabilities.drop: [ALL]`)
- `allowPrivilegeEscalation: false`, `privileged: false`
- `readOnlyRootFilesystem: true`
- `hostNetwork: false`, `hostPID: false`
- no service account token (`automountServiceAccountToken: false`) — the
  exporter never talks to the Kubernetes API
- one host mount: the `/dev/zfs` character device (the branch detection needs
  no mount — `/sys/module` is already visible)

The exporter reads pool and dataset properties. It never mounts, creates or
destroys anything.

### Scanner findings

A scan of the image reports nothing in the OS layer and a set of Go advisories
in the exporter binary itself, which is upstream's release built with an older
toolchain. Those are accepted, not fixed: rebuilding from source to clear them
would make this something other than a packaging of the upstream release. CI
blocks on the OS layer and reports the rest to the Security tab. See
[SECURITY.md](SECURITY.md).

Releases are signed with [cosign][] keyless, and carry an SPDX SBOM and build
provenance as attestations:

```console
$ cosign verify ghcr.io/tschuering/zfs-exporter:0.1.0 \
    --certificate-identity-regexp '^https://github.com/tschuering/zfs-exporter-helm/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

[cosign]: https://docs.sigstore.dev/cosign/overview/

## Install

```console
$ helm install zfs-exporter oci://ghcr.io/tschuering/charts/zfs-exporter \
    --namespace monitoring \
    --create-namespace
```

On a cluster where only some nodes have ZFS, constrain it — otherwise the pods
on the others sit there reporting failures:

```yaml
nodeSelector:
  node-role.kubernetes.io/storage: "true"
```

### Scraping

Two options, neither on by default:

```yaml
# Prometheus Operator
serviceMonitor:
  enabled: true
  labels:
    release: prometheus
```

```yaml
# Agents that discover by pod annotation (Grafana Alloy, the
# prometheus.io/scrape convention)
prometheusAnnotations:
  enabled: true
```

The Service is headless on purpose. A DaemonSet's metrics differ per node, so a
load-balanced ClusterIP would report a different node's pools on every scrape.
Endpoint discovery reads every pod address instead.

### Values

See [`charts/zfs-exporter/values.yaml`](charts/zfs-exporter/values.yaml); every
key is commented. The ones most often changed:

| Key | Default | |
| --- | --- | --- |
| `image.digest` | `""` | pin here for reproducible rollouts; wins over `tag` |
| `nodeSelector` | `{}` | restrict to nodes that have ZFS |
| `extraArgs` | `[]` | e.g. `--collector.dataset-snapshot`, `--pool=rpool` |
| `serviceMonitor.enabled` | `false` | needs the Prometheus Operator CRD |
| `devZfsHostPath` | `/dev/zfs` | |
| `zfsUserlandVersion` | `""` | override branch detection, e.g. `"2.4"` |

## Alerting

The exporter answers `200` whether or not it could read anything, so a
successful scrape is not a working exporter. Alert on the collector instead:

```yaml
- alert: ZFSExporterCollectorFailing
  expr: zfs_scrape_collector_success{collector="pool"} == 0
  for: 15m
```

A node without the ZFS module keeps the pod `Running` and every collector at
`0` — deliberately, so it alerts rather than crash-looping.

## Development

```console
$ make build     # build for the host architecture
$ make smoke     # run it and scrape /metrics
$ make lint      # hadolint, shellcheck, yamllint
$ make chart     # helm lint, render, kubeconform
$ make scan      # trivy, failing on HIGH and CRITICAL
```

Moving to a new upstream release:

```console
$ hack/update-upstream.sh 2.4.2
```

That downloads each architecture's archive, hashes it locally, and rewrites
`checksums.txt`, the Dockerfile `ARG` and the chart's `appVersion`. Review the
digests against upstream before merging. Renovate can raise the version bump
but cannot compute the digests, so a bump that skips this script fails the
build rather than shipping something unverified.

## Licensing

This packaging is MIT (see [LICENSE](LICENSE)).

It builds an image containing software under other licences:

- `zfs_exporter` — MIT, © the upstream authors
- OpenZFS userland (`zpool`, `zfs`, `libzfs`) — CDDL-1.0
- glibc and the Debian base — LGPL-2.1-or-later and others

The image redistributes CDDL-licensed binaries. The corresponding source is
Debian trixie's `zfs-linux` package.
