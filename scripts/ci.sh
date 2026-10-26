#!/usr/bin/env bash
# Lowpath's CI pipeline for Linux and macOS hosts: everything that doesn't
# need Windows (format, vet, tests of the portable packages, cross builds,
# UI). On Windows use scripts/ci.ps1.
set -euo pipefail
cd "$(dirname "$0")/.."

stage() { printf '\n==> %s\n' "$1"; }

stage format
unformatted=$(gofmt -l cmd internal scripts ui/*.go)
if [ -n "$unformatted" ]; then echo "Not gofmt'd:"; echo "$unformatted"; exit 1; fi

stage addrs
go run ./scripts/docaddrs

stage vet
go vet ./...

stage test
go test ./...

stage cross
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64 windows/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build -trimpath -o "dist/cross/${target/\//-}/" ./cmd/lowpath ./cmd/lowpathsvc ./cmd/relay ./cmd/lpcheck
done

# tree_state is what git status shows, or "-" outside a git work tree (a
# source archive), where nothing is compared.
tree_state() { git status --porcelain --untracked-files=all 2>/dev/null || echo -; }

if command -v npm >/dev/null; then
  stage ui
  # The build must leave git status as it found it: its output goes to
  # ui/dist/app, which git ignores, never to a tracked file.
  before=$(tree_state)
  (cd ui && { [ -d node_modules ] || npm ci; } && npm run build)
  [ -f ui/dist/app/index.html ] || { echo "npm run build wrote no ui/dist/app/index.html"; exit 1; }
  after=$(tree_state)
  if [ "$before" = - ]; then
    echo "Not a git work tree: not checking that the build left git status as it was."
  elif [ "$after" != "$before" ]; then
    echo "npm run build changed what git status shows (build output belongs in ui/dist/app):"
    printf '%s\n' "$after" | grep -vxF -e "$before" || true
    exit 1
  fi
fi

echo
echo "CI passed."
