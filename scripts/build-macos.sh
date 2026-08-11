#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="${ROOT_DIR}/dist"

mkdir -p "${DIST_DIR}"

cd "${ROOT_DIR}"

# Always builds for the host architecture, so the output needs no architecture
# suffix; release packages carry one, and those come from package-macos.sh.
PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig:/usr/local/lib/pkgconfig}"
export PKG_CONFIG_PATH

CGO_ENABLED=1 GOOS=darwin go build \
  -p 2 \
  -trimpath -ldflags="-s -w" \
  -o "${DIST_DIR}/djonehub-macos" ./cmd/djonehub-macos

echo "macOS binary written to ${DIST_DIR}/djonehub-macos"
