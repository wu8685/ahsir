#!/bin/sh
# Build the ahsir / ahsir-agent binaries from the bundled source on the user's
# machine, caching the result so it only happens once per (version, os, arch).
#
# Why this exists: the plugin no longer ships pre-compiled per-platform binaries
# (they were ~20 MB each × 4 platforms × 2 binaries and blew past the repo's
# single-file size limits). Instead the full Go source + vendored deps live in
# plugin/src/, and we compile on first use. Go is a hard prerequisite — see the
# README "Quick start" prerequisites.
#
# Contract:
#   - Idempotent + fast: if the cached binaries already exist, this is a no-op.
#   - Progress/diagnostics go to STDERR.
#   - The cache bin directory (containing `ahsir` and `ahsir-agent`) is printed
#     to STDOUT as the single line of "real" output, so callers can:
#         dir=$(build-binaries.sh) && exec "$dir/ahsir" "$@"
#   - Concurrent callers (SessionStart hook + wrapper) are serialized with an
#     atomic mkdir lock and an atomic temp-dir → final mv, so a half-built
#     binary is never observed.
set -e

plugin_root=$(cd "$(dirname "$0")" && pwd)
src_dir="$plugin_root/src"
manifest="$plugin_root/.claude-plugin/plugin.json"

# Version: single source of truth is the plugin manifest, matching the Makefile.
version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" 2>/dev/null | head -1)
[ -n "$version" ] || version=dev

# Normalize uname → GOOS/GOARCH strings.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "ahsir: unsupported OS '$os' (need darwin or linux)" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "ahsir: unsupported arch '$arch' (need amd64 or arm64)" >&2; exit 1 ;;
esac

# Cache keyed by version so a plugin update transparently triggers a rebuild.
# Overridable for tests / unusual setups via AHSIR_CACHE_DIR.
cache_root=${AHSIR_CACHE_DIR:-$HOME/.cache/ahsir}
bin_dir="$cache_root/$version/bin/$os-$arch"

# Fast path: already built.
if [ -x "$bin_dir/ahsir" ] && [ -x "$bin_dir/ahsir-agent" ]; then
  echo "$bin_dir"
  exit 0
fi

# Go is required to build.
if ! command -v go >/dev/null 2>&1; then
  echo "ahsir: 'go' not found on PATH — the ahsir plugin compiles its binaries" >&2
  echo "       on first use, so a Go toolchain is required. Install Go (https://go.dev/dl/)" >&2
  echo "       and re-run, or build manually: cd '$src_dir' && go build ./cmd/ahsir" >&2
  exit 1
fi

# Serialize concurrent builds (SessionStart hook can race the wrapper).
lock="$cache_root/.build.lock"
mkdir -p "$cache_root"
i=0
while ! mkdir "$lock" 2>/dev/null; do
  # Another build is in flight; wait for it, then take the fast path.
  i=$((i + 1))
  if [ "$i" -gt 600 ]; then
    echo "ahsir: timed out waiting for an in-progress build (stale lock? rm -rf '$lock')" >&2
    exit 1
  fi
  sleep 1
  if [ -x "$bin_dir/ahsir" ] && [ -x "$bin_dir/ahsir-agent" ]; then
    echo "$bin_dir"
    exit 0
  fi
done
# Release the lock no matter how we exit.
trap 'rmdir "$lock" 2>/dev/null || true' EXIT INT TERM

# Re-check under lock — a racing builder may have finished while we waited.
if [ -x "$bin_dir/ahsir" ] && [ -x "$bin_dir/ahsir-agent" ]; then
  echo "$bin_dir"
  exit 0
fi

echo "ahsir: building binaries for $os-$arch (v$version) — first run only…" >&2

ldflags="-X github.com/wu8685/ahsir/internal/version.Version=$version"
tmp_dir="$bin_dir.tmp.$$"
rm -rf "$tmp_dir"
mkdir -p "$tmp_dir"

# Build from the vendored bundle: offline, deterministic, no network needed.
(
  cd "$src_dir"
  GO111MODULE=on CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -mod=vendor -ldflags "$ldflags" -o "$tmp_dir/ahsir"       ./cmd/ahsir
  GO111MODULE=on CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -mod=vendor -ldflags "$ldflags" -o "$tmp_dir/ahsir-agent" ./cmd/ahsir-agent
) >&2

# Atomic publish: move the finished pair into place.
mkdir -p "$(dirname "$bin_dir")"
rm -rf "$bin_dir"
mv "$tmp_dir" "$bin_dir"

echo "ahsir: built → $bin_dir" >&2
echo "$bin_dir"
