#!/usr/bin/env bash
# End-to-end demo: 1 coordinator, 3 nodes, upload, kill a node, verify repair + download.
set -euo pipefail
cd "$(dirname "$0")/.."
W=$(mktemp -d); BIN=$W/bin; mkdir -p "$BIN"
go build -o "$BIN" ./cmd/...
export DRIVE_SERVER=http://localhost:18080
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

"$BIN/coordinator" -listen :18080 -state "$W/state.json" -chunk-mb 1 -offline-after 8s >"$W/coord.log" 2>&1 &
sleep 1
export DRIVE_TOKEN=$("$BIN/drive" register alice | sed 's/.*=//')
for i in 1 2 3; do
  "$BIN/node" -coordinator $DRIVE_SERVER -listen :1900$i -dir "$W/node$i" -capacity-mb 100 >"$W/node$i.log" 2>&1 &
  eval "PID$i=$!"
done
sleep 1
"$BIN/drive" stats

head -c 5500000 /dev/urandom >"$W/big.bin"
"$BIN/drive" upload "$W/big.bin"
ID=$("$BIN/drive" ls | awk '{print $1}')
"$BIN/drive" get "$ID" "$W/out.bin" && cmp "$W/big.bin" "$W/out.bin" && echo "OK: download matches"

echo "--- killing node 1, waiting for repair"
kill $PID1; sleep 22
"$BIN/drive" nodes
"$BIN/drive" get "$ID" "$W/out2.bin" && cmp "$W/big.bin" "$W/out2.bin" && echo "OK: still readable"
grep -c repair "$W/coord.log" || true
"$BIN/drive" rm "$ID"; "$BIN/drive" stats
