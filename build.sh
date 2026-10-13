#!/usr/bin/env bash
# Cross-compile release binaries into dist/ (no cgo, single static files).
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
for t in linux/amd64 linux/arm64 windows/amd64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  for c in p2pdrive coordinator; do
    # -H=windowsgui would hide the console; we keep it so the access link is visible
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "dist/$c-$os-$arch$ext" ./cmd/$c
  done
done
ls -lh dist
