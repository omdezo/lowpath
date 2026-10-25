#!/usr/bin/env bash
# Lowpath's CI pipeline for Linux and macOS hosts: everything that doesn't
# need Windows (format, vet, tests of the portable packages, cross builds,
# UI). On Windows use scripts/ci.ps1.
set -euo pipefail
cd "$(dirname "$0")/.."

stage() { printf '\n==> %s\n' "$1"; }

stage format
unformatted=$(gofmt -l cmd internal)
if [ -n "$unformatted" ]; then echo "Not gofmt'd:"; echo "$unformatted"; exit 1; fi

stage vet
go vet ./...

stage test
go test ./...

stage cross
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do
  GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build -trimpath -o "dist/cross/${target/\//-}/" ./cmd/relay ./cmd/lpcheck
done

if command -v npm >/dev/null; then
  stage ui
  (cd ui && { [ -d node_modules ] || npm ci; } && npm run build)
fi

echo
echo "CI passed."
