#!/usr/bin/env bash
# Cross-build deck for a Termux (Android) device that has no Go toolchain.
#
#   ./deploy/build-android.sh                 # build ./deck-android-arm64
#   ./deploy/build-android.sh mate10          # build and push to a device
#
# IMPORTANT: the target must be GOOS=android, not linux. Android's seccomp
# policy kills the process (SIGSYS) when a linux/arm64 Go binary calls
# pidfd_open, which the Go runtime does on the first exec.Command — so a
# linux/arm64 build crashes the moment deck spawns a model.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT="deck-android-arm64"
echo "→ building $OUT (GOOS=android GOARCH=arm64, CGO_ENABLED=0)"
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$OUT" ./cmd/deck
ls -la "$OUT"

if [ "${1:-}" != "" ]; then
  HOST="$1"
  echo "→ pushing to $HOST"
  scp "$OUT" "$HOST:~/modeldeck/deck.new"
  ssh "$HOST" 'cd ~/modeldeck && mv deck.new deck && chmod 755 deck && touch deck && ./deck version && ./start-termux.sh restart'
  echo "→ done. The device rebuilds locally once 'pkg install golang' is available."
fi
