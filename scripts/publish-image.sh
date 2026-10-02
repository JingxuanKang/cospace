#!/bin/bash
# Publish the space base image under the tag the daemon pins (internal/dist
# BaseImage). The multi-arch build (linux/arm64 + linux/amd64) runs on GitHub
# Actions (.github/workflows/space-image.yml) and pushes to ghcr.io with the
# workflow's own token. Bump the tag whenever image/ changes, so existing
# daemons keep pulling the image they were tested with.
#
#   scripts/publish-image.sh            run the workflow on master and wait for it
#   scripts/publish-image.sh --force    same, overwriting an existing tag
#   scripts/publish-image.sh --build    local arm64 build only (test with -image <ref>)
set -euo pipefail
cd "$(dirname "$0")/.."
ref=$(sed -n 's/.*BaseImage = "\(.*\)"/\1/p' internal/dist/dist.go)
[ -n "$ref" ] || { echo "publish-image: BaseImage not found in internal/dist/dist.go" >&2; exit 1; }

if [ "${1:-}" = "--build" ]; then
  container build --platform linux/arm64 -t "$ref" -f image/Dockerfile image/
  echo "built $ref locally"
  exit 0
fi

force=false
[ "${1:-}" = "--force" ] && force=true
gh workflow run space-image.yml --ref master -f force="$force"
sleep 5
run=$(gh run list --workflow space-image.yml --limit 1 --json databaseId -q '.[0].databaseId')
echo "publishing $ref — https://github.com/JingxuanKang/cospace/actions/runs/$run"
gh run watch "$run" --exit-status
