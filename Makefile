.PHONY: build console test unit e2e infra lint dist clean

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

# Release tarballs (docs/development.md "Releases"): lux_<version>_linux_
# {arm64,amd64}.tar.gz (luxd, lux, both runner arches' lux-runner/lux-shim),
# lux_<version>_darwin_{arm64,amd64}.tar.gz (lux only), and SHA256SUMS, all
# in dist/. Static, trimmed, versioned from VERSION (the release tag).
dist: console
	VERSION=$(VERSION) ./scripts/dist.sh

clean:
	rm -rf bin dist node_modules console/node_modules packages/*/node_modules packages/*/dist && find console/dist -mindepth 1 ! -name .keep -delete
