#!/bin/sh
# CoSpace host installer — turns this machine into a CoSpace host.
#   curl -fsSL https://jingxuankang.github.io/cospace/host.sh | sh
#
# macOS (Apple Silicon, macOS 26+): installs Apple container (Homebrew, or
#   Apple's signed package) and the prebuilt cospaced, then `cospaced setup`
#   starts container services, registers a login item and opens the console.
# Linux (x86_64 / arm64): needs Docker Engine usable without sudo and systemd;
#   installs cospaced, then `cospaced setup` registers a systemd user service.
# The space image (about 600 MB) then downloads once in the background; the
# console shows its progress. Safe to re-run: it upgrades in place and keeps
# the options the installed daemon runs with.
#
# Extra arguments are passed to the daemon (`cospaced serve`) and replace the
# installed service's options, e.g. to serve the console under a public
# hostname:
#   curl -fsSL …/host.sh | sh -s -- -console-hosts console.example.com
# They are checked before the service is written; a typo fails here instead
# of leaving a daemon that restarts forever. `cospaced setup -reset` returns
# to the defaults.
set -eu

BASE="https://github.com/JingxuanKang/cospace/releases/latest/download"
CONTAINER_RELEASES="https://github.com/apple/container/releases"

say() { printf 'cospace: %s\n' "$*"; }
die() { printf 'cospace: %s\n' "$*" >&2; exit 1; }

os=$(uname -s)
arch=$(uname -m)

# --- runtime prerequisites -----------------------------------------------------
case "$os" in
  Darwin)
    [ "$arch" = arm64 ] || die "a macOS host needs an Apple Silicon Mac (Apple container does not run on Intel)."
    major=$(sw_vers -productVersion | cut -d. -f1)
    [ "$major" -ge 26 ] || die "a macOS host needs macOS 26 or later (this Mac runs $(sw_vers -productVersion))."
    goos=darwin goarch=arm64
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
    ;;
  Linux)
    case "$arch" in
      x86_64|amd64)  goarch=amd64 ;;
      aarch64|arm64) goarch=arm64 ;;
      *) die "unsupported architecture '$arch' (x86_64 or arm64 only)." ;;
    esac
    goos=linux
    command -v docker >/dev/null 2>&1 \
      || die "Docker Engine is required — install it (https://docs.docker.com/engine/install/), then re-run."
    docker info >/dev/null 2>&1 \
      || die "Docker is installed but not usable without sudo — run  sudo usermod -aG docker \$USER , log out and back in, then re-run."
    systemctl --user show-environment >/dev/null 2>&1 \
      || die "a systemd user session is required (systemctl --user); log in through a normal SSH or desktop session and re-run."
    ;;
  *)
    die "the CoSpace host runs on macOS or Linux (guests on any OS use install.sh / install.ps1)."
    ;;
esac

# --- cospaced ------------------------------------------------------------------
bindir=""
for d in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin"; do
  if [ -d "$d" ] && [ -w "$d" ]; then bindir="$d"; break; fi
done
if [ -z "$bindir" ]; then bindir="$HOME/.local/bin"; mkdir -p "$bindir"; fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "downloading cospaced ($goos/$goarch)…"
curl -fsSL "$BASE/cospaced_${goos}_${goarch}.tar.gz" | tar -xz -C "$tmp"
chmod +x "$tmp/cospaced"
mv "$tmp/cospaced" "$bindir/cospaced"
# Unsigned for now: clear the quarantine flag so Gatekeeper lets it run.
if [ "$goos" = darwin ]; then xattr -d com.apple.quarantine "$bindir/cospaced" 2>/dev/null || true; fi
ver=$("$bindir/cospaced" version 2>/dev/null || true)
if [ -n "$ver" ]; then say "installed $bindir/cospaced ($ver)"; else say "installed $bindir/cospaced"; fi

# --- service + console ---------------------------------------------------------
"$bindir/cospaced" setup -- "$@"

case ":$PATH:" in
  *":$bindir:"*) : ;;
  *) say "add $bindir to your PATH to use the cospaced CLI:  export PATH=\"$bindir:\$PATH\"" ;;
esac
if [ "$goos" = linux ]; then
  cat <<EOF

Next:
  • The console listens on this server's loopback only. From your laptop:
      ssh -L 18931:127.0.0.1:18931 $(id -un)@$(hostname)
    then open http://127.0.0.1:18931 (or reach it over Tailscale).
  • Sign in to the AI tools you want to offer on this server (claude, codex,
    grok — headless device-code login works); credentials never leave it.
  • In the console, create a space and click "Invite a Guest".
  • If this server's firewall only allows listed ports (an INPUT chain ending in
    REJECT/DROP), let spaces reach the AI gateway on the docker bridge:
      sudo iptables -I INPUT -i docker0 -p tcp --dport 18930 -j ACCEPT
EOF
else
  cat <<'EOF'

Next:
  • Sign in to the AI tools you want to offer on this Mac (claude, codex, grok) —
    spaces use them through CoSpace; the credentials never leave this Mac.
  • In the console, create a space and click "Invite a Guest".
EOF
fi
