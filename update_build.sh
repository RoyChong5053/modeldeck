#!/usr/bin/env bash
# Rebuild deck: format check, vet, build. Pass --pull to git pull first.
#
#   ./update_build.sh
#   ./update_build.sh --pull
set -euo pipefail
cd "$(dirname "$0")"

if [ "${1:-}" = "--pull" ]; then
  echo "→ git pull --ff-only"
  git pull --ff-only
fi

echo "→ gofmt -l ."
unformatted="$(gofmt -l . || true)"
if [ -n "$unformatted" ]; then
  echo "unformatted files (run: gofmt -w .):" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "→ go vet ./..."
go vet ./...

echo "→ go build"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o deck ./cmd/deck
./deck version

echo "→ validate test config"
./deck validate -config testdata/entries.test.yaml >/dev/null && echo "   validate ok"

echo
echo "✓ ./deck is up to date"
echo "  start:  ./start.sh"
echo "  tests:  ./start.sh &  && bash testdata/smoke.sh"
