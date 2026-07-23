# Build targets for ahsir.
#
# The plugin no longer ships pre-compiled per-platform binaries (they were
# ~20 MB each × 4 platforms × 2 binaries and blew past repo single-file limits).
# Instead `make plugin` syncs the buildable Go source + vendored deps into
# plugin/src/, and the plugin compiles the binaries on the user's machine on
# first use (a SessionStart hook + the bin/ wrapper call plugin/build-binaries.sh).
# Go is therefore a prerequisite for plugin users — documented in the README.

GO ?= go
BIN := bin
PLUGIN_SRC := plugin/src

# Single source of truth for the version: the plugin manifest. Stamped into the
# binary via -ldflags so `ahsir version` matches the released plugin version.
# Falls back to "dev" if the manifest can't be read.
VERSION := $(or $(shell sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' plugin/.claude-plugin/plugin.json | head -1),dev)
LDFLAGS := -X github.com/wu8685/ahsir/internal/version.Version=$(VERSION)

.PHONY: all build plugin plugin-src clean test test-ui-fast ui-test-deps test-ui-browser test-ui

all: build

# Local dev build for the current machine.
build:
	GO111MODULE=on $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/ahsir       ./cmd/ahsir
	GO111MODULE=on $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/ahsir-agent ./cmd/ahsir-agent

# Refresh the source bundle shipped inside the plugin. Run this before a release
# whenever cmd/, internal/, or dependencies change, then commit plugin/src/.
# The bundle is vendored so the on-device first-run build is offline + deterministic.
plugin: plugin-src

plugin-src:
	rm -rf $(PLUGIN_SRC)
	mkdir -p $(PLUGIN_SRC)
	cp -R cmd internal go.mod go.sum $(PLUGIN_SRC)/
	cd $(PLUGIN_SRC) && GO111MODULE=on $(GO) mod vendor
	@echo "==> synced plugin source bundle into $(PLUGIN_SRC) (vendored)"

clean:
	rm -rf $(BIN)/*

# Race detector is mandatory: scheduler/wrapper are concurrency-heavy, so a
# plain `go test` pass is not a trusted signal. Pre-merge checks must go
# through this target (or replicate the -race flag).
test:
	GO111MODULE=on $(GO) test -race -count=1 ./...

test-ui-fast:
	GO111MODULE=on $(GO) test -count=1 ./internal/ui
	cd $(PLUGIN_SRC) && GO111MODULE=on $(GO) test -count=1 ./internal/ui

ui-test-deps:
	npm ci --prefix ui-tests
	npm exec --prefix ui-tests playwright install chromium

test-ui-browser:
	npm test --prefix ui-tests

test-ui: test-ui-fast test-ui-browser
