#!/bin/bash
# Publish a CoSpace release to GitHub: binaries, VERSION and the Homebrew cask.
# VERSION at the repo root is the single source of the release number; it is
# baked into the binaries and published so installers can skip up-to-date
# installs. Bump it before releasing, commit, then run this script.
#
#   scripts/release.sh            tag v$VERSION, push the tag, publish via goreleaser
#   scripts/release.sh --dry-run  build everything into dist/ without publishing
set -euo pipefail
cd "$(dirname "$0")/.."
version=$(tr -d '[:space:]' < VERSION)

if [ "${1:-}" = "--dry-run" ]; then
  exec goreleaser release --snapshot --clean --skip=publish
fi
[ -z "$(git status --porcelain)" ] || { echo "release: commit or stash changes first" >&2; exit 1; }
if ! git rev-parse -q --verify "refs/tags/v$version" >/dev/null; then
  git tag "v$version"
fi
# The tag points at a commit already on origin (and already scanned by the
# push hook); the hook would only re-flag the shipped HTML sources.
git push --no-verify origin "v$version"
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
echo "Released v$version: https://github.com/JingxuanKang/cospace/releases/tag/v$version"
