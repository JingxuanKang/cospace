#!/bin/sh
# CoSpace guest tool installer.
#   curl -fsSL https://cospace.jingxuan.uk/install.sh | sh
# Installs the `cospace` binary for this machine into a bin dir on PATH.
# Safe to re-run: an existing install is left alone when it is already the
# published version and updated in place otherwise.
set -eu

# Release assets on GitHub; VERSION is published with every release.
BASE="https://github.com/JingxuanKang/cospace/releases/latest/download"

os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Darwin) goos=darwin ;;
  Linux)  goos=linux ;;
  *) echo "cospace: unsupported OS '$os' (macOS or Linux only)" >&2; exit 1 ;;
esac
case "$arch" in
  arm64|aarch64) goarch=arm64 ;;
  x86_64|amd64)  goarch=amd64 ;;
  *) echo "cospace: unsupported arch '$arch'" >&2; exit 1 ;;
esac

# Pairing and every later connection run the system ssh client.
if ! command -v ssh >/dev/null 2>&1 || ! command -v ssh-keygen >/dev/null 2>&1; then
  echo "cospace: an OpenSSH client (ssh, ssh-keygen) is required — install it (e.g. apt install openssh-client), then run this again" >&2
  exit 1
fi

next_steps() {
  echo "Next: run the 'cospace pair …' line your host sent you, then  ssh <space-name>"
}

# The published version; empty if the download host does not serve it.
latest=$(curl -fsSL "$BASE/VERSION" 2>/dev/null | tr -d '[:space:]' || true)

existing=$(command -v cospace 2>/dev/null || true)
if [ -n "$existing" ]; then
  # A Homebrew cask install is a symlink into the Caskroom; leave it to brew.
  target=$(readlink "$existing" 2>/dev/null || echo "$existing")
  case "$target" in
    *Caskroom*|*Cellar*)
      echo "cospace: $existing is managed by Homebrew — update with:  brew upgrade --cask cospace"
      next_steps
      exit 0 ;;
  esac
  current=$("$existing" version 2>/dev/null | awk 'NR==1 {print $2}' || true)
  if [ -n "$latest" ] && [ "$current" = "$latest" ]; then
    echo "cospace: already up to date ($current) at $existing"
    next_steps
    exit 0
  fi
  # Replace the copy that is on PATH so the update takes effect immediately.
  dir=$(dirname "$existing")
  if [ -w "$dir" ] && [ -w "$existing" ]; then
    bindir="$dir"
  else
    echo "cospace: cannot write to $existing; installing a fresh copy instead (the old one may shadow it on PATH)" >&2
  fi
  echo "cospace: updating ${current:-unknown version} → ${latest:-latest}"
fi

# Pick a writable bin dir on PATH; fall back to ~/.local/bin.
if [ -z "${bindir:-}" ]; then
  for d in /usr/local/bin "$HOME/.local/bin"; do
    if [ -d "$d" ] && [ -w "$d" ]; then bindir="$d"; break; fi
  done
fi
if [ -z "${bindir:-}" ]; then
  bindir="$HOME/.local/bin"
  mkdir -p "$bindir"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "cospace: downloading cospace ($goos/$goarch)…"
curl -fsSL "$BASE/cospace_${goos}_${goarch}.tar.gz" | tar -xz -C "$tmp"
chmod +x "$tmp/cospace"
mv "$tmp/cospace" "$bindir/cospace"
# On macOS, clear the quarantine flag so Gatekeeper allows the unsigned binary.
if [ "$goos" = darwin ]; then xattr -d com.apple.quarantine "$bindir/cospace" 2>/dev/null || true; fi
# Add the short alias `co` unless that name already belongs to something else.
alias_note=""
if [ ! -e "$bindir/co" ] || [ "$(readlink "$bindir/co" 2>/dev/null)" = "$bindir/cospace" ]; then
  ln -sf "$bindir/cospace" "$bindir/co"
  alias_note=" (alias: co)"
fi

installed=$("$bindir/cospace" version 2>/dev/null | awk 'NR==1 {print $2}' || true)
echo "cospace: installed cospace ${installed:-} to $bindir/cospace${alias_note}"
case ":$PATH:" in
  *":$bindir:"*) : ;;
  *) echo "cospace: add $bindir to your PATH:  export PATH=\"$bindir:\$PATH\"" ;;
esac
next_steps
