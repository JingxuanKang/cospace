#!/bin/sh
set -eu
# Apply a saved network boundary before accepting any guest SSH sessions.
# Fail closed: a gateway-only space must not come up without its firewall,
# and the reason must be visible in the container log, not just an exit code.
if [ -f /etc/cospace-network.nft ]; then
  if ! nft -f /etc/cospace-network.nft; then
    echo "cospace-entrypoint: could not apply /etc/cospace-network.nft; refusing to start SSH without the network boundary" >&2
    exit 1
  fi
fi
mkdir -p /run/sshd
exec /usr/sbin/sshd -D -e
