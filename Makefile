.PHONY: build console test unit e2e infra lint tf-validate host-unit host-test dist clean

GO_LDFLAGS := -s -w -X github.com/marcioapm/lux/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARIES := luxd lux-runner lux-shim lux lux-fake

# Static binaries: they run inside Fedora hosts, and the shim runs inside
# arbitrary images. luxd embeds the console (console/dist), built first.
build: console
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

# The per-package node_modules are links bun install makes into the root
# store. A checkout from before the workspace (console/ had its own install)
# keeps real copies there, and the console then bundles two Reacts and
# renders blank: so they are always remade.
console:
	rm -rf console/node_modules packages/*/node_modules
	bun install --frozen-lockfile
	bun run typecheck
	cd console && bun run build

unit:
	LUX_TEST_PG=$${LUX_TEST_PG:-postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable} go test ./...

# JOBS environments at once (each its own luxd, database and hosts):
# the whole suite in about 4 minutes at 4. JOBS=1 runs serially.
JOBS ?= 4

e2e:
	cd tests && uv run python run_tests.py -j $(JOBS)

infra:
	cd tests && uv run python run_tests.py --infra-only

test: unit e2e

lint:
	go vet ./...
	gofmt -l . | (! grep .)

# fmt, init (no backend, committed lock files), validate and the mocked
# `terraform test`s of every root under deploy/terraform. Needs
# Terraform >= 1.10; TERRAFORM=/path/to/terraform to pick one.
tf-validate:
	./scripts/tf-validate.sh

# The control host's reconciler (deploy/terraform/examples/aws/host):
# host-unit is its pytest suite (Python 3.13+, pytest); host-test adds the
# container smoke test (docker, privileged; builds `make dist` first if
# dist/ has no VERSION release for the Docker host's arch). HOST_DIR=path
# runs both against another copy of host/ (e.g. a downstream repo's).
HOST_DIR ?= deploy/terraform/examples/aws/host
PYTHON ?= python3

host-unit:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m pytest -q -p no:cacheprovider $(HOST_DIR)/tests

host-test: host-unit
	VERSION=$(or $(VERSION),v0.0.0-smoke) HOST_DIR=$(HOST_DIR) ./scripts/host-smoke.sh

# Release tarballs (docs/development.md "Releases"): lux_<version>_linux_
# {arm64,amd64}.tar.gz (luxd, lux, both runner arches' lux-runner/lux-shim),
# lux_<version>_darwin_{arm64,amd64}.tar.gz (lux only), and SHA256SUMS, all
# in dist/. Static, trimmed, versioned from VERSION (the release tag).
dist: console
	VERSION=$(VERSION) ./scripts/dist.sh

clean:
	rm -rf bin dist node_modules console/node_modules packages/*/node_modules packages/*/dist && find console/dist -mindepth 1 ! -name .keep -delete
