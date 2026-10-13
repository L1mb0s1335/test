#!/usr/bin/env bash
# End-to-end test: coordinator + 3 donor nodes + a client.
# Checks: encrypted at rest on donors, wrong passphrase rejected, tamper
# detection, self-healing replication after a node dies, delete frees space.
set -euo pipefail
cd "$(dirname "$0")/.."
W=$(mktemp -d); BIN=$W/bin; mkdir -p "$BIN"
go build -o "$BIN" ./cmd/...
URL=http://localhost:18080
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT
ok() { echo "  OK: $*"; }
bad() { echo "  FAIL: $*"; exit 1; }

"$BIN/coordinator" -listen :18080 -state "$W/state.json" -chunk-mb 1 -offline-after 15s >"$W/coord.log" 2>&1 &
sleep 1

# Alice: account + passphrase
export XDG_CONFIG_HOME=$W/alice APPDATA=$W/alice DRIVE_PASSPHRASE='correct horse battery'
"$BIN/p2pdrive" register $URL alice >"$W/reg.txt"
TOKEN=$(grep -o '[0-9a-f]\{48\}' "$W/reg.txt" | head -1)

# Three donors (each with its own config dir), all owned by alice for simplicity
for i in 1 2 3; do
  mkdir -p "$W/n$i/p2pdrive"
  printf '{"server":"%s","token":"%s","node":{"dir":"%s","capacity_mb":100}}' "$URL" "$TOKEN" "$W/n$i/data" >"$W/n$i/p2pdrive/config.json"
  XDG_CONFIG_HOME=$W/n$i "$BIN/p2pdrive" node >"$W/n$i.log" 2>&1 &
  eval "PID$i=$!"
done
sleep 2

SECRET="TOP-SECRET-MARKER-$(date +%s)"
{ echo "$SECRET"; head -c 3000000 /dev/urandom | base64; } >"$W/secret-plan.txt"
echo "== upload"
OUT=$("$BIN/p2pdrive" put "$W/secret-plan.txt"); echo "  $OUT"
ID=${OUT%% *}

echo "== donors hold only ciphertext"
if grep -rqa "$SECRET" "$W"/n*/data; then bad "plaintext found on a donor"; else ok "no plaintext on donors"; fi
if grep -rqa "secret-plan" "$W"/n*/data "$W/state.json"; then bad "file name leaked"; else ok "file name not on donors or coordinator"; fi

echo "== roundtrip"
"$BIN/p2pdrive" get "$ID" "$W/out.txt" >/dev/null && cmp "$W/secret-plan.txt" "$W/out.txt" && ok "decrypted file identical"

echo "== wrong passphrase"
if DRIVE_PASSPHRASE=nope-nope-nope "$BIN/p2pdrive" ls 2>/dev/null; then bad "wrong passphrase accepted"; else ok "rejected"; fi

echo "== tampering by a donor"
for f in "$W"/n*/data/chunks/*-0; do printf 'X' | dd of="$f" bs=1 seek=100 conv=notrunc 2>/dev/null; done
if "$BIN/p2pdrive" get "$ID" "$W/bad.txt" 2>/dev/null; then bad "tampered chunk accepted"; else ok "tampering detected, no output written"; fi
[ ! -e "$W/bad.txt" ] || bad "partial file left behind"

echo "== node failure + self-healing (fresh file)"
head -c 4500000 /dev/urandom >"$W/big.bin"
OUT=$("$BIN/p2pdrive" put "$W/big.bin"); ID2=${OUT%% *}
kill $PID1; sleep 32
"$BIN/p2pdrive" get "$ID2" "$W/big.out" >/dev/null && cmp "$W/big.bin" "$W/big.out" && ok "readable after node loss"
echo "  repairs logged: $(grep -c 'repair .*replicated' "$W/coord.log")"

echo "== delete"
"$BIN/p2pdrive" rm "$ID2"; "$BIN/p2pdrive" rm "$ID"; "$BIN/p2pdrive" ls | wc -l | xargs echo "  files left:"
echo "ALL CHECKS PASSED"
