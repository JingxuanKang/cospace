#!/bin/bash
# Build the space base image and push it to ghcr.io under the tag the daemon
# pins (internal/dist BaseImage). Bump that tag whenever image/ changes, so
# existing daemons keep pulling the image they were tested with.
#
#   scripts/publish-image.sh            build + push
#   scripts/publish-image.sh --build    build only (local test as `-image <ref>`)
set -euo pipefail
cd "$(dirname "$0")/.."
ref=$(sed -n 's/.*BaseImage = "\(.*\)"/\1/p' internal/dist/dist.go)
[ -n "$ref" ] || { echo "publish-image: BaseImage not found in internal/dist/dist.go" >&2; exit 1; }

container build --platform linux/arm64 -t "$ref" -f image/Dockerfile image/
[ "${1:-}" = "--build" ] && { echo "built $ref"; exit 0; }

# ghcr.io needs a token with write:packages:  gh auth refresh -s write:packages
gh auth token | container registry login ghcr.io --username "$(gh api user -q .login)" --password-stdin
container image push "$ref"
echo "pushed $ref — make the package public once: GitHub → Packages → cospace-base → Settings → Change visibility"
