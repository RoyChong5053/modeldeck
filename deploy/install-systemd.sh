#!/usr/bin/env bash
# Install (or refresh) the systemd **user** unit for this checkout and start it.
#
#   ./deploy/install-systemd.sh          install + enable + start
#   ./deploy/install-systemd.sh --no-start   install + enable only
#
# Requirements: systemd with a user session (loginctl enable-linger keeps it
# running after logout). Phones (Termux) have no systemd — use start-termux.sh.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG="${MODELDECK_CONFIG:-$HOME/.config/modeldeck/entries.yaml}"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT="$UNIT_DIR/modeldeck.service"
START=1
[ "${1:-}" = "--no-start" ] && START=0

command -v systemctl >/dev/null 2>&1 || { echo "systemd not available (Termux? use ./start-termux.sh)" >&2; exit 1; }

# The access token lives in the project folder (.env) — one file per device,
# easy to inspect and to copy to a new machine.
"$REPO/deck" token --write -config "$CONFIG" >/dev/null 2>&1 || true
ENV_FILE="$REPO/.env"
if [ ! -f "$ENV_FILE" ]; then
  echo "warning: $ENV_FILE not found; the deck will start without a token" >&2
fi

mkdir -p "$UNIT_DIR"
cat > "$UNIT" <<EOF
[Unit]
Description=modeldeck model scheduler
Documentation=https://github.com/RoyChong5053/modeldeck
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$REPO
EnvironmentFile=-$ENV_FILE
ExecStart=$REPO/deck serve -config $CONFIG
Restart=always
RestartSec=2
Environment=HOME=$HOME
Environment=PATH=/usr/local/bin:/usr/bin:/bin

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable modeldeck.service
if [ "$START" = "1" ]; then
  systemctl --user restart modeldeck.service
  sleep 1
  systemctl --user --no-pager --lines=5 status modeldeck.service || true
fi

echo
echo "unit   : $UNIT"
echo "config : $CONFIG"
echo "token  : $ENV_FILE"
grep -E '^DECK_TOKEN=' "$ENV_FILE" 2>/dev/null || true
echo "log    : journalctl --user -u modeldeck -f"
echo "linger : $(loginctl show-user "$USER" -p Linger 2>/dev/null | cut -d= -f2 || echo '?') (enable with: sudo loginctl enable-linger $USER)"
