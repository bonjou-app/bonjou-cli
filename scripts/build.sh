#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"
BIN_DIR="$DIST_DIR/bin"
mkdir -p "$BIN_DIR"

pushd "$ROOT_DIR" >/dev/null

echo "Building Bonjou binaries..."
GOOS=linux GOARCH=amd64 go build -o "$BIN_DIR/bonjou-linux-amd64" ./cmd/bonjou
GOOS=linux GOARCH=arm64 go build -o "$BIN_DIR/bonjou-linux-arm64" ./cmd/bonjou
GOOS=darwin GOARCH=arm64 go build -o "$BIN_DIR/bonjou-macos" ./cmd/bonjou
GOOS=windows GOARCH=amd64 go build -o "$BIN_DIR/bonjou.exe" ./cmd/bonjou

# The web coordinator is server-side only, so it ships for Linux alone. Deploy
# it with scripts/deploy-relay.sh.
echo "Building Bonjou Web coordinator..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o "$BIN_DIR/bonjou-relay-linux-amd64" ./cmd/bonjou-relay
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
    -o "$BIN_DIR/bonjou-relay-linux-arm64" ./cmd/bonjou-relay

echo "Build artifacts stored in $BIN_DIR"
popd >/dev/null
