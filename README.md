# zfs-exporter

A container image and Helm chart for [pdf/zfs_exporter][upstream]. The exporter
reports per-dataset and per-pool OpenZFS metrics to Prometheus.

Upstream publishes release binaries, but no image and no chart. This repository
supplies both. The image is distroless and carries only the exporter and the
OpenZFS userland that the exporter runs. The chart deploys a DaemonSet and
gives the exporter the one piece of host access that it needs.

[upstream]: https://github.com/pdf/zfs_exporter

## Motivation

I wanted to run this exporter the way I already run node_exporter: as a
DaemonSet, deployed from the same place as everything else. I also wanted to
keep the Ubuntu host below it as thin as possible.

Everything else on my node moved into Kubernetes first. The ZFS exporter was
the last part of the monitoring stack that stayed on the host. It needed a
binary in `/usr/local/bin`, a systemd unit, a version pin and a checksum.
Configuration management carried all four, and each one needed its own review
and its own upgrade path. That is a set of host state for one exporter.

That is a poor trade. I can rebuild a host that runs only a kernel, ZFS and a
kubelet. Each package and unit above that layer is one more thing to patch, one
more thing that drifts, and one more thing to recreate when the machine is
reinstalled. In the cluster, the exporter gets the same deployment, rollback
and upgrade path as every other workload. It also removes the last service from
the host.

It stayed on the host longer than everything else for one concrete reason.
Upstream publishes no image and no chart, and this exporter needs the OpenZFS
userland next to it, which node_exporter does not. This repository fills that
gap.

## Why an image is not trivial

`zfs_exporter` does not read `/proc/spl/kstat`, which is what node_exporter's
ZFS collector does. It runs `zpool(8)` and `zfs(8)`, and it parses their
output:

```go
exec.Command(`zpool`, `get`, `-Hpo`, `name,property,value`, ...)
exec.Command(`zfs`,   `get`, `-Hprt`, ...)
```

The image must therefore carry the OpenZFS userland. That userland communicates
with the **host's** kernel module through `/dev/zfs`.

## Host compatibility

**OpenZFS 2.3.x and 2.4.x, from the same tag.** The image carries a userland
for each branch and selects one at run time. One DaemonSet therefore serves a
fleet that runs both branches, and an upgrade of a node from one branch to the
other needs no change here.

`cmd/zfs-shim` is installed on `PATH` under two names, `zpool` and `zfs`. These
are the names that the exporter executes. The shim reads the node's branch from
`/sys/module/zfs/version`. A container can read that file with no mount,
because module state is not namespaced. The shim then starts the matching tree:

```
/opt/zfs/2.4/lib/ld-linux-x86-64.so.2 \
    --library-path /opt/zfs/2.4/lib /opt/zfs/2.4/sbin/zpool ...
```

Each tree carries its own libraries **and its own dynamic loader**. Two
userlands built against different glibc versions can therefore live in one
image. 2.3 links `libzfs.so.6` and 2.4 links `libzfs.so.7`, and neither is
installed at a standard path. Nothing is guessed. A node on a branch that the
image does not carry gets this message:

```
zfs-shim: no bundled OpenZFS userland for host version 2.5; this image carries 2.3, 2.4
```

It does not get an unclear ioctl failure. `zfsUserlandVersion` in the chart
replaces the detection.

To add a branch, add a builder stage and a base image. See the `zfs23` and
`zfs24` stages in the [Dockerfile](Dockerfile). Each stage checks the branch
that its base image supplied. A base image that moves to a new OpenZFS branch
therefore fails the build. It does not silently change which branches the image
supports.

## What is in the image

The base is `gcr.io/distroless/base-debian13`. It has no shell, no package
manager and no apt state.

| Path | Source |
| --- | --- |
| `/usr/local/bin/zfs_exporter` | upstream release, sha256 pinned in `checksums.txt` |
| `/usr/local/bin/{zpool,zfs}` | symlinks to `zfs-shim`, built from `cmd/zfs-shim` |
| `/opt/zfs/2.3/` | Debian trixie `zfsutils-linux`, with its libraries and loader |
| `/opt/zfs/2.4/` | Debian forky `zfsutils-linux`, likewise |

The image is about 84 MB. It is built for `linux/amd64` and `linux/arm64`.

The build verifies the exporter binary against digests held in this repository.
It does not use the `sha256sums.txt` that upstream serves next to the release.
A sums file in the same directory as the artifact shares the trust root of that
artifact, and anyone who can replace one file can replace the other. A digest
held here is reviewable in a diff instead. `hack/update-upstream.sh` downloads
and hashes each archive, and rewrites the digest for each architecture.

## Security posture

The exporter runs as a **non-root uid** with **no capabilities**. It reads pool
and dataset properties, and the kernel refuses it anything else.

That is not a statement of intent. It is how OpenZFS controls its own ioctls:

```c
zfs_secpolicy_read(...)  { return (0); }                            // stats: no privilege
secpolicy_sys_config(cr) { return priv_policy(cr, CAP_SYS_ADMIN, EPERM); }
secpolicy_zfs(cr)        { return priv_policy(cr, CAP_SYS_ADMIN, EACCES); }
```

The exporter runs `zpool list`, `zpool get` and `zfs get`. OpenZFS registers
all three with `zfs_secpolicy_read`. Every ioctl that changes state uses one of
the other two functions. With no capabilities, the reads succeed and
`zfs destroy` returns `permission denied`. The kernel returns that error. No
check in this repository produces it.

The exporter therefore runs with `runAsUser: 65532`, `runAsNonRoot: true`,
`privileged: false`, `capabilities.drop: [ALL]`,
`seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem: true`, no host
namespaces, **no host mounts at all**, and no service account token. It meets
the **restricted** Pod Security Standard.

Only `runAsUser` and `runAsGroup` are values. The chart renders the rest, and
`runAsNonRoot` follows `runAsUser`, so the two can never disagree.

Measured in a running container:

```
Uid:        65532  65532  65532  65532
CapEff:     0000000000000000
CapBnd:     0000000000000000
NoNewPrivs: 1
Seccomp:    2      (SECCOMP_MODE_FILTER)
```

Root is not needed, because OpenZFS ships `90-zfs.rules`, which sets
`/dev/zfs` to mode `0666`. Every uid passes the permission check on the device
node, and OpenZFS itself decides what each ioctl may do.

**One host needs a change.** That udev rule carries
`OPTIONS+="static_node=zfs"`. Where udev never ran, the kernel creates
`/dev/zfs` at mode `0600 root:root` and nothing relaxes it. The exporter then
stops at `open()` with `permission denied`. Run `ls -l /dev/zfs` on the node.
If it reads `crw-------`, set:

```yaml
runAsUser: 0
runAsGroup: 0
```

`runAsNonRoot` becomes `false` with them, and every other setting stays.

### Why the device plugin exists

**A container cannot open a device node, whatever the file permissions say.**
That is the whole reason. Two separate rules guard a device, and they are easy
to confuse:

| | what it asks | who satisfies it |
| --- | --- | --- |
| Device cgroup | May this container open device `10:249` at all? | **the device plugin** |
| File permissions | May this uid read and write that node? | OpenZFS, with a `0666` udev rule |

A `hostPath` mount makes `/dev/zfs` *visible* in the container. Visible is not
openable. Kubernetes never adds a hostPath device to the cgroup allowlist, so
the first rule still refuses.

That is measurable. A container that is **root**, with `/dev/zfs` mounted and
the node at `0666` — every permission check already satisfied — still gets:

```console
$ dd if=/dev/zfs bs=1 count=0
dd: can't open '/dev/zfs': Operation not permitted
```

`Operation not permitted` there is the device cgroup. No file mode and no uid
can answer it.

Only two things can:

1. **`privileged: true`** — unlocks every device, and also grants the full
   capability set and disables seccomp.
2. **A device plugin** — asks the kubelet to add one device to one container.

The chart uses the second (`cmd/zfs-device-plugin`), and that choice is what
keeps every other setting real:

| | `privileged: true` | device plugin |
| --- | --- | --- |
| `CapEff` | `000001ffffffffff` | `0000000000000000` |
| Seccomp | disabled | `RuntimeDefault` active |
| Runs as | root | uid 65532 |
| `zfs destroy` | permitted by the kernel | `EPERM` |

The plugin brokers access; it never uses the device. Its `Allocate` returns a
description of the node, and the **kubelet** performs the insertion and writes
the cgroup rule. The exporter then opens `/dev/zfs` itself, as its own uid.

The plugin needs `runAsUser: 0`, because the kubelet plugin directory is mode
`0755 root:root` and only uid 0 can create the socket there. It does **not**
need `privileged`: measured on a node, it registers and serves with
`CapEff: 0000000000000000` and seccomp active. **No pod in this chart sets
`privileged: true`.**

It is a separate DaemonSet, because the kubelet admits a pod only after the
resource that the pod requests is registered. A plugin in the exporter's own
pod could never start: the pod would wait for itself.

### Read-only where that works

Three accesses exist here, and each one gets the narrowest mode that works:

| access | mode | reason |
| --- | --- | --- |
| plugin reads `/dev/zfs` | `readOnly: true` | it calls `stat()` and never opens the device |
| plugin writes the kubelet directory | read-write | it creates and removes its own socket |
| exporter opens `/dev/zfs` | `rw` | libzfs opens with `O_RDWR` even to list a pool |

The third row looks wrong and is not. A read-only cgroup rule fails at
`open()`, because libzfs needs `O_RDWR` for every operation, and `zpool list`
is one of them.

Write access in the cgroup rule does not permit a write to a pool. The two are
separate mechanisms. OpenZFS checks `CAP_SYS_ADMIN` inside each mutating ioctl,
and this container holds no capability, so the kernel refuses `zfs destroy`
with `EPERM`. See `Permissions` in `cmd/zfs-device-plugin/main.go`.

### The plugin decides the namespace's security level

The plugin mounts the kubelet plugin directory, and the **baseline** Pod
Security Standard forbids a hostPath volume. Its namespace can therefore only
enforce **privileged**, while the exporter satisfies **restricted**. In one
namespace the weaker level covers both.

`devicePlugin.namespace` separates them. The chart renders no Namespace object
and Helm creates none, so create it first:

```console
$ kubectl create namespace zfs-system
$ kubectl label namespace zfs-system pod-security.kubernetes.io/enforce=privileged
$ helm install zfs-exporter oci://ghcr.io/tschuering/charts/zfs-exporter \
    --namespace monitoring --set devicePlugin.namespace=zfs-system
```

The extended resource is a node property and not a namespace property, so the
exporter still receives the device from another namespace.

### A second thing the plugin buys you

The plugin advertises the resource only on nodes that have `/dev/zfs`. The
scheduler therefore places the exporter only there. You maintain no
`nodeSelector`, and no pod stays `Running` while every collector fails.

### Scanner findings

A scan of the image reports no finding in the OS layer. Every finding sits in
the exporter binary, which is upstream's release built with Go 1.24.2. A scan on
2026-08-31 reported 75 open alerts there, and 38 of those were HIGH or
CRITICAL. `govulncheck` reported 33 of the 75 as reachable, and all 33 come
from the standard library.

These advisories are accepted, not fixed. A rebuild from source clears them,
and it also makes this image something other than a packaging of the upstream
release. [pdf/zfs_exporter#71][upstream-issue] asks upstream to move to
a supported Go release, and that change is prepared and tested. CI fails on
findings in the OS layer, and reports the other findings to the Security tab.
See [SECURITY.md](SECURITY.md).

[upstream-issue]: https://github.com/pdf/zfs_exporter/issues/71

Cosign signs each release keylessly. Each release also carries an SPDX SBOM and
build provenance as attestations:

```console
$ cosign verify ghcr.io/tschuering/zfs-exporter:0.1.0 \
    --certificate-identity-regexp '^https://github.com/tschuering/zfs-exporter-helm/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

[cosign]: https://docs.sigstore.dev/cosign/overview/

## Versions and tags

One git tag builds both the image and the chart, and both carry the same
version. A release tagged `v0.1.0` publishes:

| Artifact | Tag | Moves? |
| --- | --- | --- |
| image | `0.1.0` | no |
| image | `0.1` | yes, within the minor |
| image | `latest` | yes |
| chart | `0.1.0` | no |

Each tag also creates a GitHub release. That release carries the generated
changelog, the packaged chart as a `.tgz`, and the SBOM as a file. This is for
anyone who prefers not to use a registry client.

There is deliberately **no `2.4.1` image tag**. That number belongs to the
exporter, and it is the one number that cannot separate two releases of this
packaging. Two releases can package the same upstream version and still differ
in base image, ZFS userland or a security rebuild. `appVersion` in the chart
records which exporter is in the image. The version you deploy is this
project's own.

Set `image.digest` for a rollout that cannot change under you.

## Install

From the OCI registry:

```console
$ helm install zfs-exporter oci://ghcr.io/tschuering/charts/zfs-exporter \
    --namespace monitoring \
    --create-namespace
```

A classic Helm repository is also available, if it suits your tooling better.
Both paths serve the same chart. The repository index points at the tarballs
attached to the GitHub releases:

```console
$ helm repo add zfs-exporter https://tschuering.github.io/zfs-exporter-helm
$ helm repo update
$ helm install zfs-exporter zfs-exporter/zfs-exporter \
    --namespace monitoring \
    --create-namespace
```

On a cluster where only some nodes have ZFS, you constrain nothing. The plugin
advertises the resource only where `/dev/zfs` exists. The scheduler therefore
places the exporter only on those nodes.

### Scraping

Two options are available. Neither is enabled by default:

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

The Service is headless on purpose. A DaemonSet reports different metrics on
each node. A load-balanced ClusterIP would therefore report a different node's
pools on each scrape. Endpoint discovery reads every pod address instead.

### Restricting who may scrape

The metrics show pool layout, dataset names and capacities. Treat the endpoint
as sensitive. The chart can render a `NetworkPolicy`, which is **disabled by
default**:

```yaml
networkPolicy:
  enabled: true
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

It is disabled by default for two reasons:

1. A `NetworkPolicy` has an effect only if the cluster's CNI enforces policies.
   Several CNIs do not. A policy with no effect is worse than no policy,
   because a reader sees it as protection.
2. On a cluster that *does* enforce policies, a policy that names the wrong
   source stops your scraper from reaching the exporter.

Note what an empty `ingressFrom` does. The rendered policy permits the metrics
port **from anywhere**, and denies every other port. That limits the ports, not
the clients. Set `ingressFrom` to whatever actually scrapes this exporter.
Without that, the policy gives you very little.

The policy denies every outbound connection, and that part takes no
configuration. The exporter forks `zpool` and `zfs` on the node and serves
HTTP. It opens no connection of its own and resolves no name, so it needs no
egress rule, and not even one for DNS.

### Values

See [`charts/zfs-exporter/values.yaml`](charts/zfs-exporter/values.yaml). Every
key has a comment. These are the keys that change most often:

| Key | Default | |
| --- | --- | --- |
| `image.digest` | `""` | pin here for reproducible rollouts. Overrides `tag` |
| `extraArgs` | `[]` | e.g. `--collector.dataset-snapshot`, `--pool=rpool` |
| `serviceMonitor.enabled` | `false` | needs the Prometheus Operator CRD |
| `networkPolicy.enabled` | `false` | see [Restricting who may scrape](#restricting-who-may-scrape) |
| `devicePlugin.resourceName` | `tschuering.github.io/dev-zfs` | change only on a collision |
| `devicePlugin.namespace` | `""` | put the plugin elsewhere, so this namespace can enforce a stricter PSA level |
| `devicePlugin.kubeletPluginDir` | `/var/lib/kubelet/device-plugins` | if your kubelet root differs |
| `zfsUserlandVersion` | `""` | override branch detection, e.g. `"2.4"` |
| `runAsUser` | `65532` | set to `0` where `/dev/zfs` is `0600`, see [Security posture](#security-posture) |

## Alerting

The exporter answers `200` even if it read nothing. A successful scrape is
therefore not proof of a working exporter. Alert on the collector instead:

```yaml
- alert: ZFSExporterCollectorFailing
  expr: zfs_scrape_collector_success{collector="pool"} == 0
  for: 15m
```

On a node without the ZFS module, the pod stays `Running` and every collector
reports `0`. That is deliberate. The pod raises an alert instead of a crash
loop.

## Development

```console
$ make build     # build for the host architecture
$ make smoke     # run it and scrape /metrics
$ make lint      # hadolint, shellcheck, yamllint
$ make chart     # helm lint, render, kubeconform
$ make scan      # trivy, failing on the OS layer
```

`make scan` runs the same split as CI. A finding in the OS layer fails it,
because the base image and the packages above it are this repository's
decision. The upstream binary is reported and does not fail the target, because
no change here can correct it. See [Scanner findings](#scanner-findings).

To move to a new upstream release:

```console
$ hack/update-upstream.sh 2.4.2
```

The script downloads the archive for each architecture and hashes it locally.
It then rewrites `checksums.txt`, the Dockerfile `ARG` and the chart's
`appVersion`. Compare the digests with upstream before you merge. Renovate can
raise the version bump, but it cannot compute the digests. A bump that skips
this script therefore fails the build, instead of publishing something
unverified.

## Licensing

This packaging is MIT (see [LICENSE](LICENSE)).

It builds an image that contains software under other licences:

- `zfs_exporter` — MIT, © the upstream authors
- OpenZFS userland (`zpool`, `zfs`, `libzfs`) — CDDL-1.0
- glibc and the Debian base — LGPL-2.1-or-later and others

The image redistributes CDDL-licensed binaries. The corresponding source is
Debian trixie's `zfs-linux` package.
