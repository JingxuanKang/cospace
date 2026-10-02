#!/bin/bash
# Real end-to-end dogfood of the tailcat transport + pairing + cospace guest flow,
# exactly as a real guest would experience it (minus running on another machine).
set -euo pipefail
cd "$(dirname "$0")/.."

SPACE=dogfood
DATA=$(mktemp -d)
KEYDIR=$(mktemp -d)
FAKEHOME=$(mktemp -d)   # pretend guest home so cospace writes an isolated ssh config
PORT=${PORT:-18930}
GWPID=""

cleanup() {
  ./bin/cospaced space delete "$SPACE" -data "$DATA" 2>/dev/null || true
  [ -n "$GWPID" ] && kill "$GWPID" 2>/dev/null || true
  rm -rf "$DATA" "$KEYDIR" "$FAKEHOME"
}
trap cleanup EXIT
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

step "build + serve (gateway + transport + idle reaper)"
go build -o bin/cospaced ./cmd/cospaced
go build -o bin/cospace ./cmd/cospace
if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
  echo "port $PORT is already in use (a production cospaced?); stop it or set PORT to a free port" >&2
  exit 1
fi
./bin/cospaced serve -listen 0.0.0.0:$PORT -data "$DATA" -idle-timeout 0 -gateway-url "http://192.168.64.1:$PORT" &
GWPID=$!
sleep 1

step "seed gateway credential"
ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT/anthropic claude -p "reply with exactly: SEED" --model haiku | grep -q SEED

step "create space + issue invite (one-time code over tailcat addr)"
./bin/cospaced space create "$SPACE" -memory 2 -cpus 2 -data "$DATA"
ssh-keygen -q -t ed25519 -f "$KEYDIR/guest" -N "" -C dogfood-guest
INVITE=$(./bin/cospaced invite "$SPACE" -ttl 10m -data "$DATA" | grep 'cospace pair')
echo "invite line: $INVITE"
ADDR=$(echo "$INVITE" | awk '{print $3}')
CODE=$(echo "$INVITE" | awk '{print $4}')
[ -n "$ADDR" ] && [ -n "$CODE" ] || { echo "bad invite line"; exit 1; }

step "wait for the space's transport node to converge on DERP"
# space create runs in a separate process; serve reconciles transport on a
# ~10s loop, and the tailcat node then needs a few seconds to reach DERP.
PAIRED=""
for i in $(seq 1 12); do
  if HOME="$FAKEHOME" ./bin/cospace pair "$ADDR" "$CODE" -name dogfood-guest -key "$KEYDIR/guest.pub" -alias "$SPACE" 2>/tmp/pair.err; then
    PAIRED=1; break
  fi
  sleep 5
done
[ -n "$PAIRED" ] || { echo "pairing never converged:"; tail -3 /tmp/pair.err; exit 1; }
grep -q "Host $SPACE" "$FAKEHOME/.ssh/config" || { echo "ssh config block not written"; cat "$FAKEHOME/.ssh/config"; exit 1; }
echo "paired + ssh config written"

step "second redemption of the same code must fail (single-use)"
if HOME="$FAKEHOME" ./bin/cospace pair "$ADDR" "$CODE" -name evil -key "$KEYDIR/guest.pub" -alias x 2>/dev/null; then
  echo "one-time code was reusable"; exit 1
fi

step "guest ssh into the space THROUGH tailcat (ProxyCommand = cospace connect)"
PATH="$PWD/bin:$PATH" HOME="$FAKEHOME" \
  ssh -F "$FAKEHOME/.ssh/config" -i "$KEYDIR/guest" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=40 \
  "$SPACE" 'echo TAILCAT-OK: $(whoami)@$(hostname); source /etc/profile.d/cospace.sh; claude -p "reply with exactly: E2E-OK" --model haiku' \
  | tee /tmp/dogfood.out
grep -q 'TAILCAT-OK: space@' /tmp/dogfood.out
grep -q 'E2E-OK' /tmp/dogfood.out

printf '\n\033[1;32mDOGFOOD OK — guest reached the space over tailcat and ran claude through the gateway\033[0m\n'
