#!/usr/bin/env bash
# modeldeck launcher: build if needed, then run in the foreground.
#
#   ./start.sh              use ~/.config/modeldeck/entries.yaml
#   MODELDECK_CONFIG=... ./start.sh
#   KEEP=1 ./start.sh       leave running models alone on exit
#
# Ctrl+C stops deck; by default it also stops the models it started.
set -euo pipefail
cd "$(dirname "$0")"

CONFIG="${MODELDECK_CONFIG:-$HOME/.config/modeldeck/entries.yaml}"
BIN="${DECK_BIN:-./deck}"

if [ ! -x "$BIN" ] || [ -n "$(find cmd internal go.mod go.sum -newer "$BIN" -print -quit 2>/dev/null)" ]; then
  echo "→ building $BIN …"
  CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/deck
fi

if [ ! -f "$CONFIG" ]; then
  echo "config not found: $CONFIG" >&2
  echo "create one with:  ./deck scan -o \"$CONFIG\"" >&2
  echo "or copy the example: cp examples/entries.example.yaml \"$CONFIG\"" >&2
  exit 1
fi

ARGS=(serve -config "$CONFIG")
if [ "${KEEP:-0}" = "1" ]; then
  ARGS+=(-keep)
fi

echo "→ $BIN ${ARGS[*]}"
echo "   config : $CONFIG"
echo "   WebUI  : http://127.0.0.1:12688"
echo "   Ctrl+C to stop${KEEP:+ (KEEP=$KEEP)}"
exec "$BIN" "${ARGS[@]}"
