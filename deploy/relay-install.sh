#!/bin/bash
# One-shot relay deployment onto the host's own VPS (Debian/Ubuntu, systemd).
# Usage: deploy/relay-install.sh user@vps-host
# Prints the relay address + token to feed `cospaced serve -relay ...`.
set -euo pipefail
DEST=${1:?usage: relay-install.sh user@vps-host}
cd "$(dirname "$0")/.."

# Build for the VPS's own architecture (Oracle Ampere / Hetzner CAX are arm64).
ARCH=$(ssh "$DEST" uname -m)
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "unsupported VPS architecture: $ARCH" >&2; exit 1 ;;
esac
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -o /tmp/cospace-relay ./cmd/cospace-relay
TOKEN=$(openssl rand -hex 24)

scp /tmp/cospace-relay "$DEST":/tmp/cospace-relay
# The token travels over the ssh channel on stdin, not on a remote command line.
printf '%s\n' "$TOKEN" | ssh "$DEST" "sudo install -m755 /tmp/cospace-relay /usr/local/bin/cospace-relay \
  && sudo install -d -m700 -o nobody -g nogroup /var/lib/cospace-relay \
  && sudo tee /etc/cospace-relay.token >/dev/null \
  && sudo chown nobody:nogroup /etc/cospace-relay.token && sudo chmod 600 /etc/cospace-relay.token \
  && sudo tee /etc/systemd/system/cospace-relay.service >/dev/null <<'UNIT'
[Unit]
Description=CoSpace relay (dumb TCP pipe)
After=network.target
[Service]
ExecStart=/usr/local/bin/cospace-relay
User=nobody
Group=nogroup
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/cospace-relay
PrivateTmp=yes
[Install]
WantedBy=multi-user.target
UNIT
  sudo systemctl daemon-reload && sudo systemctl enable --now cospace-relay"

HOST=${DEST#*@}
echo
echo "Relay deployed on $HOST:7300 (token in /etc/cospace-relay.token on the VPS)."
echo "Open TCP 7300 + 24300-24800 in the VPS firewall/security group."
echo
echo "NOTE: the relay transport is implemented and tested (internal/relay) but is"
echo "not yet selectable in 'cospaced serve'; guests connect over the default"
echo "tailcat transport. See DESIGN.md §14."
