#!/usr/bin/env bash
set -euo pipefail

# Match CI's Go, editor/SDK integration, and package checks on Node 24.
node ./scripts/build-web.js
go build -trimpath -o px0 .
go test -v ./...
npm ci --prefix sdk
(
  cd sdk
  PX0_BIN=../px0 npm run check
  npm run test:package
)
