#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026 gao-gao-zai and contributors.
# Run on the tdog host after loading /srv/build-server/env/workbuddy2api.sh.
# Docker packages these already-built artifacts; it does not compile the app.
set -euo pipefail
cd "$(dirname "$0")/.."
out="${1:?usage: bash scripts/build-release.sh /absolute/artifact/directory}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
export GOMAXPROCS="${GOMAXPROCS:-4}"
stage="$(mktemp -d "${TMPDIR:-/tmp}/workbuddy2api-release.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
(
  cd react
  npm ci --ignore-scripts --no-audit --no-fund
  npm rebuild esbuild --ignore-scripts=false
  npm run build -- --outDir ../frontend --emptyOutDir
)
# Rebuilding an unpacked source release already has the vendored sources.
if [[ -f vendor/modules.txt ]]; then
  vendor_dir="$PWD/vendor"
else
  go mod vendor -o "$stage/vendor"
  vendor_dir="$stage/vendor"
fi
manifest_args=()
if [[ -n "${SOURCE_FILES_MANIFEST:-}" ]]; then
  manifest_args=(--manifest "$SOURCE_FILES_MANIFEST")
fi
python3 scripts/package-source.py --root "$PWD" --vendor-dir "$vendor_dir" \
  --output "$out/source.tar.gz" "${manifest_args[@]}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p=2 -trimpath -o "$out/wb2api" ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p=2 -trimpath -o "$out/login" ./cmd/login
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p=2 -trimpath -o "$out/handoff-gateway" ./cmd/handoff-gateway
cp LICENSE NOTICE "$out/"
cp -R LICENSES "$out/"
cp Dockerfile Dockerfile.local Dockerfile.gateway "$out/"
cp -R frontend "$out/"
(cd "$out" && sha256sum wb2api login handoff-gateway source.tar.gz > SHA256SUMS)
