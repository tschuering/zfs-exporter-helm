# Local equivalents of the checks that CI runs. Nothing here pushes.

IMAGE ?= zfs-exporter
TAG   ?= dev
PLATFORM ?= linux/$(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
CHART := charts/zfs-exporter
# OpenZFS branches that the image carries. Keep these in step with the builder
# stages in the Dockerfile.
ZFS_TREES ?= 2.3 2.4

.PHONY: help build smoke lint chart scan clean

help:
	@awk 'BEGIN{FS=":.*##"} /^[a-z-]+:.*##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the image for the host architecture
	docker build --platform $(PLATFORM) -t $(IMAGE):$(TAG) .

smoke: build ## Run the image and scrape it
	@set -e; \
	docker run --rm --platform $(PLATFORM) $(IMAGE):$(TAG) --version; \
	docker rm -f zfs-exporter-smoke >/dev/null 2>&1 || true; \
	docker run -d --name zfs-exporter-smoke --platform $(PLATFORM) \
		-p 9134:9134 $(IMAGE):$(TAG) >/dev/null; \
	for i in $$(seq 1 30); do \
		curl -fsS http://127.0.0.1:9134/metrics -o /tmp/zfs-metrics && break; \
		sleep 1; \
	done; \
	docker rm -f zfs-exporter-smoke >/dev/null; \
	grep -q '^zfs_exporter_build_info' /tmp/zfs-metrics; \
	echo "ok: $$(wc -l < /tmp/zfs-metrics) metric lines"; \
	for tree in $(ZFS_TREES); do \
		docker run --rm --platform $(PLATFORM) --entrypoint zpool \
			-e ZFS_USERLAND_VERSION=$$tree $(IMAGE):$(TAG) version 2>&1 \
			| grep -qE 'auto-loaded|ZFS Module|zfs-'"$$tree" \
			&& echo "ok: $$tree userland runs" \
			|| { echo "FAIL: $$tree userland"; exit 1; }; \
	done

lint: ## hadolint, shellcheck, yamllint and the standard go checks
	hadolint Dockerfile
	shellcheck hack/*.sh
	yamllint --strict .
	gofmt -s -l ./cmd
	go vet ./...
	go mod tidy -diff
	go mod verify
	go tool govulncheck ./...
	go test -race ./...

chart: ## helm lint, then render and schema-check each ci/ values file
	helm lint $(CHART) --strict
	@for values in $(CHART)/ci/*.yaml; do \
		echo "== $$values"; \
		helm template ci $(CHART) --namespace monitoring --values "$$values" \
			| kubeconform -strict -summary -schema-location default \
				-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'; \
	done

# This scans the whole image, so it is red on a clean tree: the upstream
# exporter binary carries Go advisories that no change here can correct. CI
# splits the scan and blocks only on the OS layer. See SECURITY.md.
scan: build ## Trivy scan. Fails on HIGH and CRITICAL
	trivy image --ignore-unfixed --severity HIGH,CRITICAL --exit-code 1 $(IMAGE):$(TAG)

clean:
	docker rmi -f $(IMAGE):$(TAG) 2>/dev/null || true
