#!/bin/bash
# End-to-end smoke test against the real Apple container runtime.
#
# Prereqs: `container system start` done, cospace-base image built,
# claude logged in on this Mac (its credential seeds the gateway).
# Everything it creates is torn down at the end.
set -euo pipefail
cd "$(dirname "$0")/.."

SPACE=smoke-space
DATA=$(mktemp -d)
KEYDIR=$(mktemp -d)
PORT=${PORT:-18930}
GWPID=""

cleanup() {
  ./bin/cospaced space delete "$SPACE" -data "$DATA" 2>/dev/null || true
  [ -n "$GWPID" ] && kill "$GWPID" 2>/dev/null || true
  rm -rf "$DATA" "$KEYDIR"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

step "build"
mkdir -p bin
go build -o bin/cospaced ./cmd/cospaced

step "gateway up on :$PORT"
if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
  echo "port $PORT is already in use (a production cospaced?); stop it or set PORT to a free port" >&2
  exit 1
fi
./bin/cospaced serve -listen 0.0.0.0:$PORT -data "$DATA" -gateway-url "http://192.168.64.1:$PORT" &
GWPID=$!
sleep 1

step "seed gateway with host credential (one local claude call)"
ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT/anthropic \
  claude -p "reply with exactly: SEED" --model haiku | grep -q SEED

step "create space"
./bin/cospaced space create "$SPACE" -memory 2 -cpus 2 -data "$DATA"

step "add member"
ssh-keygen -q -t ed25519 -f "$KEYDIR/guest" -N "" -C smoke-guest
./bin/cospaced member add "$SPACE" smoke-guest "$KEYDIR/guest.pub" -data "$DATA"

step "resolve space IP"
IP=$(./bin/cospaced space list -data "$DATA" | awk -v r="$SPACE" '$1==r{print $3}')
[ -n "$IP" ] && [ "$IP" != "-" ] || { echo "no IP for $SPACE"; exit 1; }
echo "space at $IP"

SSH="ssh -i $KEYDIR/guest -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 space@$IP"

step "guest ssh reaches the space"
$SSH 'echo shell-ok: $(whoami)@$(hostname)' | grep -q 'shell-ok: space@'

step "guest workspace is the mounted volume"
$SSH 'touch /workspace/smoke-file && ls /workspace' | grep -q smoke-file
ls "$DATA/spaces/$SPACE/workspace" | grep -q smoke-file

step "claude works inside the space through the gateway (fake token only)"
$SSH 'source /etc/profile.d/cospace.sh
      [ "${ANTHROPIC_AUTH_TOKEN#cs_}" != "$ANTHROPIC_AUTH_TOKEN" ] || { echo "token not a space token"; exit 1; }
      claude -p "reply with exactly: SPACE-OK" --model haiku' | grep -q SPACE-OK

step "revoked member loses ssh"
./bin/cospaced member revoke "$SPACE" smoke-guest -data "$DATA"
if $SSH -o PasswordAuthentication=no true 2>/dev/null; then
  echo "revoked key still works"; exit 1
fi

step "stop/start keeps files, IP may change"
./bin/cospaced space stop "$SPACE" -data "$DATA"
./bin/cospaced space start "$SPACE" -data "$DATA"
sleep 2
IP2=$(./bin/cospaced space list -data "$DATA" | awk -v r="$SPACE" '$1==r{print $3}')
./bin/cospaced member add "$SPACE" smoke-guest "$KEYDIR/guest.pub" -data "$DATA" >/dev/null
ssh -i "$KEYDIR/guest" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 \
  space@"$IP2" 'ls /workspace' | grep -q smoke-file

step "delete purges"
./bin/cospaced space delete "$SPACE" -data "$DATA"
[ ! -d "$DATA/spaces/$SPACE" ]

printf '\n\033[1;32mSMOKE OK\033[0m\n'
