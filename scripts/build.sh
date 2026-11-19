#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
npm ci --prefix webapp
npm run typecheck --prefix webapp
npm run build --prefix webapp
mkdir -p server/dist dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "server/dist/plugin-linux-$arch" ./server
done
python3 scripts/package.py
