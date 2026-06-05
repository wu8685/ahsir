# Build targets for ahsir.
#
# The `plugin` target produces Claude Code plugin bundles under plugin/bin/<os>-<arch>/
# so the whole plugin/ directory can be distributed as a single Claude Code plugin
# (via marketplace or `claude --plugin-dir ./plugin`). Users don't need Go installed
# — the binary is already there.

GO ?= go
BIN := bin
PLUGIN_BIN := plugin/bin

# Platforms we ship binaries for. Append more as needed.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: all build plugin plugin-current clean test

all: build

# Local dev build for the current machine.
build:
	$(GO) build -o $(BIN)/ahsir       ./cmd/ahsir
	$(GO) build -o $(BIN)/ahsir-agent ./cmd/ahsir-agent

# Build for ALL plugin target platforms. Slow (4 cross-compiles × 2 binaries),
# used for releases. For day-to-day plugin testing on the host machine, use
# `make plugin-current` instead.
plugin: $(PLATFORMS:%=plugin/%)

# Build for just the host platform — useful when developing the plugin and
# you want the plugin tree to "just work" on your own machine without paying
# the full cross-compile cost.
plugin-current:
	@os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	  $(MAKE) plugin/$$os/$$arch

# Pattern rule: plugin/<os>/<arch> → cross-compile both binaries into the
# plugin's per-platform subdir.
plugin/%:
	@os=$$(echo $* | cut -d/ -f1); arch=$$(echo $* | cut -d/ -f2); \
	  outdir=$(PLUGIN_BIN)/$$os-$$arch; \
	  mkdir -p $$outdir; \
	  echo "==> $$os-$$arch"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -o $$outdir/ahsir       ./cmd/ahsir; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -o $$outdir/ahsir-agent ./cmd/ahsir-agent

clean:
	rm -rf $(BIN)/* $(PLUGIN_BIN)/*

test:
	$(GO) test ./...
