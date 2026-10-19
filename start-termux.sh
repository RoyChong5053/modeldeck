#!/data/data/com.termux/files/usr/bin/bash
# modeldeck launcher for Termux (Android).
#
#   ./start-termux.sh              build if needed, then serve in the foreground
#   BG=1 ./start-termux.sh         serve in the background (survives ssh logout)
#   ./start-termux.sh status       health + live entry table
#   ./start-termux.sh logs         follow the background log
#   ./start-termux.sh stop         stop deck (and the models it started)
#
# Environment:
#   DECK_LISTEN=0.0.0.0:12688      bind address written into the config
#   DECK_TOKEN=...                 optional bearer token (LAN safety)
#   MODELDECK_CONFIG=...           config path (default ~/.config/modeldeck/entries.yaml)
#   KEEP=1                         leave started models running on exit
#   BG=1                           run detached with nohup
#
# Termux notes: a wake lock is taken automatically; Android may still kill the
# process when the battery is low, so keep the phone charging for long runs.
set -euo pipefail
cd "$(dirname "$0")"

CONFIG="${MODELDECK_CONFIG:-$HOME/.config/modeldeck/entries.yaml}"
BIN="${DECK_BIN:-./deck}"
LISTEN="${DECK_LISTEN:-0.0.0.0:12688}"
STATE_DIR="${MODELDECK_STATE_DIR:-$HOME/.local/state/modeldeck}"
LOG="$STATE_DIR/deck.log"
PIDFILE="$STATE_DIR/deck.pid"

export PATH="$PREFIX/bin:$HOME/bin:/data/data/com.termux/files/usr/bin:$PATH"

say() { printf '→ %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

wake_lock() {
  have termux-wake-lock && termux-wake-lock || true
}
wake_unlock() {
  have termux-wake-unlock && termux-wake-unlock || true
}

# Rewrite server.listen / server.token in the YAML config (only those lines).
patch_server() {
  local file="$1" listen="$2" token="${3:-}" indent
  # Keep the indentation the file already uses for server keys (2 or 4 spaces).
  indent="  "
  if grep -q '^[[:space:]]*state_dir:' "$file"; then
    indent="$(sed -n 's/^\([[:space:]]*\)state_dir:.*/\1/p' "$file" | head -n 1)"
  fi
  if grep -q '^[[:space:]]*listen:' "$file"; then
    sed -i "s|^[[:space:]]*listen:.*|${indent}listen: $listen|" "$file"
  else
    sed -i "/^server:/a\\${indent}listen: $listen" "$file"
  fi
  if [ -n "$token" ]; then
    if grep -q '^[[:space:]]*token:' "$file"; then
      sed -i "s|^[[:space:]]*token:.*|${indent}token: $token|" "$file"
    else
      sed -i "/^[[:space:]]*listen:/a\\${indent}token: $token" "$file"
    fi
  fi
}

# All deck processes serving this config (pidfile can go stale after a failed
# bind or an Android kill, so never trust it alone).
deck_pids() {
  {
    if [ -f "$PIDFILE" ]; then
      cat "$PIDFILE" 2>/dev/null || true
    fi
    pgrep -f "deck serve -config $CONFIG" 2>/dev/null || true
  } | awk 'NF && $1 != "'"$$"'"' | sort -u
}

is_running() {
  local pid
  for pid in $(deck_pids); do
    kill -0 "$pid" 2>/dev/null && return 0
  done
  return 1
}

health_url() {
  local host="${LISTEN%:*}" port="${LISTEN##*:}"
  case "$host" in
    0.0.0.0|::|"[::]"|"") host=127.0.0.1 ;;
  esac
  printf 'http://%s:%s/health' "$host" "$port"
}

api_base() {
  local host="${LISTEN%:*}" port="${LISTEN##*:}"
  case "$host" in
    0.0.0.0|::|"[::]"|"") host=127.0.0.1 ;;
  esac
  printf 'http://%s:%s' "$host" "$port"
}

lan_ip() {
  local ip=""
  if have ip; then
    ip="$(ip -o -4 addr show 2>/dev/null | awk '$2 ~ /^(wlan|ap|rmnet|eth)/ {print $4}' | cut -d/ -f1 | head -1 || true)"
  fi
  # Android blocks netlink for `ip` on many devices; termux-api can still tell us.
  if [ -z "$ip" ] && have termux-wifi-connectioninfo; then
    ip="$(termux-wifi-connectioninfo 2>/dev/null | sed -n 's/.*"ip"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)"
  fi
  printf '%s' "$ip"
}

build_if_needed() {
  if [ ! -x "$BIN" ] || [ -n "$(find cmd internal go.mod go.sum -newer "$BIN" -print -quit 2>/dev/null)" ]; then
    have go || die "Go not found: run 'pkg install golang' in Termux"
    say "building $BIN (android/$(go env GOARCH)) …"
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN" ./cmd/deck
  fi
}

ensure_config() {
  mkdir -p "$(dirname "$CONFIG")" "$STATE_DIR"
  if [ ! -f "$CONFIG" ]; then
    say "config not found: $CONFIG"
    say "generating it by scanning ~/LLM (deck init) …"
    "$BIN" init -config "$CONFIG" || die "deck init failed"
    echo
  fi
  patch_server "$CONFIG" "$LISTEN" "${DECK_TOKEN:-}"
}

start_foreground() {
  build_if_needed
  ensure_config
  if is_running; then
    die "deck already running (pids: $(deck_pids | tr '\n' ' ')); use './start-termux.sh stop'"
  fi

  wake_lock
  trap 'wake_unlock' EXIT

  local args=(serve -config "$CONFIG")
  [ "${KEEP:-0}" = "1" ] && args+=(-keep)

  say "$BIN ${args[*]}"
  say "config : $CONFIG"
  say "listen : $LISTEN"
  local ip; ip="$(lan_ip)"
  if [ -n "$ip" ]; then say "LAN    : http://$ip:${LISTEN##*:}/"; fi
  say "WebUI  : $(api_base)/"
  if [ -n "${DECK_TOKEN:-}" ]; then say "token  : $DECK_TOKEN"; fi
  say "Ctrl+C to stop${KEEP:+ (KEEP=$KEEP)}"
  echo

  # Not exec: keep the EXIT trap so the wake lock is released.
  "$BIN" "${args[@]}"
}

start_background() {
  build_if_needed
  ensure_config
  if is_running; then
    die "deck already running (pids: $(deck_pids | tr '\n' ' ')); use './start-termux.sh stop'"
  fi

  wake_lock
  local args=(serve -config "$CONFIG")
  [ "${KEEP:-0}" = "1" ] && args+=(-keep)

  say "starting in background …"
  nohup "$BIN" "${args[@]}" >>"$LOG" 2>&1 &
  local pid=$!
  echo "$pid" >"$PIDFILE"
  disown "$pid" 2>/dev/null || true

  local url
  url="$(health_url)"
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    if curl -fsS --max-time 1 "$url" >/dev/null 2>&1; then
      say "up (pid $pid)"
      say "log    : $LOG"
      say "config : $CONFIG"
      say "listen : $LISTEN"
      local ip; ip="$(lan_ip)"
      if [ -n "$ip" ]; then say "LAN    : http://$ip:${LISTEN##*:}/"; fi
      say "WebUI  : $(api_base)/"
      if [ -n "${DECK_TOKEN:-}" ]; then say "token  : $DECK_TOKEN"; fi
      return 0
    fi
    sleep 0.2
  done
  rm -f "$PIDFILE"
  sleep 1
  tail -n 30 "$LOG" >&2
  die "deck exited during startup (port already in use?)"
}

stop_deck() {
  local pids pid
  pids="$(deck_pids | tr '\n' ' ')"
  if [ -n "${pids// /}" ]; then
    say "stopping deck (pids: $pids) …"
    for pid in $pids; do
      kill -TERM "$pid" 2>/dev/null || true
    done
    for _ in $(seq 1 50); do
      local alive=0
      for pid in $pids; do
        kill -0 "$pid" 2>/dev/null && alive=1
      done
      [ "$alive" = "0" ] && break
      sleep 0.2
    done
    for pid in $pids; do
      if kill -0 "$pid" 2>/dev/null; then
        say "force killing pid $pid"
        kill -KILL "$pid" 2>/dev/null || true
      fi
    done
  else
    say "deck is not running"
  fi
  rm -f "$PIDFILE"
  wake_unlock
}

show_status() {
  local url
  url="$(health_url)"
  if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
    say "deck is up at $(api_base)/"
    "$BIN" ls -config "$CONFIG" 2>/dev/null || true
  else
    say "deck is not responding at $url"
    is_running && say "pid $(cat "$PIDFILE") exists but health check failed" || true
    return 1
  fi
}

# BG=1 is shorthand for the `bg` subcommand.
if [ "${BG:-0}" = "1" ]; then
  set -- bg
fi

case "${1:-start}" in
  start) start_foreground ;;
  stop) stop_deck ;;
  restart) stop_deck; start_background ;;
  status) show_status ;;
  logs)
    [ -f "$LOG" ] || die "no log yet: $LOG"
    tail -n 200 -f "$LOG"
    ;;
  -d|bg|background) start_background ;;
  -h|--help|help)
    sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
    ;;
  *) die "unknown command: $1 (try: start | stop | restart | status | logs | bg)" ;;
esac
