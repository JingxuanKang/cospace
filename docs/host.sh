#!/bin/sh
# CoSpace host installer — turns this Mac into a CoSpace host.
#   curl -fsSL https://jingxuankang.github.io/cospace/host.sh | sh
#
# 1. checks for an Apple Silicon Mac on macOS 26+
# 2. installs Apple container (Homebrew, or Apple's signed package)
# 3. installs the prebuilt cospaced daemon (no Go toolchain needed)
# 4. runs `cospaced setup`: starts container services, registers the daemon
#    as a login item, and opens the console in your browser
# The space image (several hundred MB) then downloads once in the background;
# the console shows its progress. Safe to re-run: it upgrades in place.
#
# Extra arguments are passed to the daemon (`cospaced serve`), e.g. to serve
# the console under a public hostname:
#   curl -fsSL …/host.sh | sh -s -- -console-hosts console.example.com
set -eu

BASE="https://github.com/JingxuanKang/cospace/releases/latest/download"
CONTAINER_RELEASES="https://github.com/apple/container/releases"

say() { printf 'cospace: %s\n' "$*"; }
die() { printf 'cospace: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Darwin ] || die "the CoSpace host runs on macOS (guests can be on any OS — they use install.sh)."
[ "$(uname -m)" = arm64 ] || die "the CoSpace host needs an Apple Silicon Mac (Apple container does not run on Intel)."
major=$(sw_vers -productVersion | cut -d. -f1)
[ "$major" -ge 26 ] || die "the CoSpace host needs macOS 26 or later (this Mac runs $(sw_vers -productVersion))."

# --- Apple container ---------------------------------------------------------
if ! command -v container >/dev/null 2>&1; then
  if command -v brew >/dev/null 2>&1; then
    say "installing Apple container with Homebrew…"
    brew install container
  else
    say "installing Apple container from Apple's signed package (asks for your password)…"
    tag=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$CONTAINER_RELEASES/latest" | sed 's|.*/tag/||')
    [ -n "$tag" ] || die "could not find the latest Apple container release; install it from $CONTAINER_RELEASES and re-run."
    pkg=$(mktemp -d)/container.pkg
    curl -fsSL -o "$pkg" "$CONTAINER_RELEASES/download/$tag/container-$tag-installer-signed.pkg" \
      || die "could not download Apple container $tag; install it from $CONTAINER_RELEASES and re-run."
    sudo installer -pkg "$pkg" -target /
    rm -f "$pkg"
  fi
fi
command -v container >/dev/null 2>&1 || die "Apple container is not on PATH; open a new terminal and re-run."

# --- cospaced ------------------------------------------------------------------
bindir=""
for d in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin"; do
  if [ -d "$d" ] && [ -w "$d" ]; then bindir="$d"; break; fi
done
if [ -z "$bindir" ]; then bindir="$HOME/.local/bin"; mkdir -p "$bindir"; fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "downloading cospaced…"
curl -fsSL "$BASE/cospaced_darwin_arm64.tar.gz" | tar -xz -C "$tmp"
chmod +x "$tmp/cospaced"
mv "$tmp/cospaced" "$bindir/cospaced"
# Unsigned for now: clear the quarantine flag so Gatekeeper lets it run.
xattr -d com.apple.quarantine "$bindir/cospaced" 2>/dev/null || true
say "installed $bindir/cospaced"

# --- service + console ---------------------------------------------------------
"$bindir/cospaced" setup -- "$@"

case ":$PATH:" in
  *":$bindir:"*) : ;;
  *) say "add $bindir to your PATH to use the cospaced CLI:  export PATH=\"$bindir:\$PATH\"" ;;
esac
cat <<'EOF'

Next:
  • Sign in to the AI tools you want to offer on this Mac (claude, codex, grok) —
    spaces use them through CoSpace; the credentials never leave this Mac.
  • In the console, create a space and click "Invite a Guest".
EOF
