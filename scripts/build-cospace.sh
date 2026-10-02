#!/bin/bash
# Cross-compile the guest tool `cospace` for every platform a guest might use,
# into dist/. Hand a guest the one matching their OS — it is a single file,
# no installer, no dependencies.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
version=$(tr -d '[:space:]' < VERSION)

build() {
  local os=$1 arch=$2 out=$3
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/$out" ./cmd/cospace
  echo "  dist/$out  ($(du -h "dist/$out" | cut -f1))"
}

echo "Building cospace for guests:"
build darwin arm64 cospace-macos-arm64      # Apple Silicon Macs
build darwin amd64 cospace-macos-intel      # Intel Macs
build linux  amd64 cospace-linux            # Linux
build windows amd64 cospace-windows-amd64.exe # Windows x86_64
build windows arm64 cospace-windows-arm64.exe # Windows ARM64
echo "Done. Send a guest the file matching their computer, plus the 'cospace pair …' line from 'cospaced invite'."
