#!/usr/bin/env bash
set -euo pipefail

export CGO_ENABLED=0 GOARCH=amd64
version=$(cat VERSION)
for target in linux windows; do
  binary=sshore
  if [[ "$target" == windows ]]; then binary=sshore.exe; fi
  GOOS="$target" go build -trimpath -ldflags="-s -w -X main.version=$version" -o "dist/$binary" .
  python3 scripts/package.py --binary "dist/$binary" --version "$version" --os "$target" --arch "$GOARCH"
done
