#!/usr/bin/env bash
# Smoke test for modeldeck against testdata/entries.test.yaml.
# Requires: deck serve already running on $BASE, curl, python3.
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:12688}"
CONFIG="${CONFIG:-testdata/entries.test.yaml}"
STATE_DIR="${STATE_DIR:-/tmp/opencode/modeldeck-state}"
DECK="${DECK:-/tmp/opencode/deck}"
# The deck only reads $DECK_TOKEN from the environment, so the test instance
# stays unauthenticated even when the project folder has a .env.
unset DECK_TOKEN
pass=0
fail=0

ck() { # ck <name> <condition-exit-code>
  if [ "$2" -eq 0 ]; then
    echo "PASS  $1"; pass=$((pass + 1))
  else
    echo "FAIL  $1"; fail=$((fail + 1))
  fi
}

jget() { # jget <json> <python-expr-on-d>
  python3 -c "import json,sys; d=json.loads(sys.argv[1]); print($2)" "$1" 2>/dev/null
}

chat() { # chat <model> -> prints assistant content
  curl -s "$BASE/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
}

state_of() { # state_of <id>
  curl -s "$BASE/api/status" | python3 -c "
import json,sys
d=json.load(sys.stdin)
for e in d['entries']:
    if e['id']=='$1': print(e['state']); break
else: print('missing')
" 2>/dev/null
}

echo "== modeldeck smoke test =="
echo "BASE=$BASE CONFIG=$CONFIG"
echo

# 1. health
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/health")
[ "$code" = "200" ]; ck "GET /health -> 200" $?

# 2. models list
models=$(curl -s "$BASE/v1/models")
echo "$models" | grep -q '"alpha"'; ck "/v1/models lists alpha" $?
echo "$models" | grep -q '"alpha-alias"'; ck "/v1/models lists alias" $?

# 3. cold start + chat via primary id
out=$(chat alpha)
[ "$(jget "$out" "d['choices'][0]['message']['content']")" = "hello from alpha" ]
ck "chat model=alpha returns backend reply (cold start)" $?
[ "$(state_of alpha)" = "ready" ]; ck "alpha is ready after first request" $?

# 4. alias routing
out=$(chat a)
echo "$out" | grep -q "hello from alpha"; ck "alias 'a' routes to alpha" $?

# 5. port lane: gamma shares alpha's port -> alpha must be evicted
out=$(chat gamma)
[ "$(jget "$out" "d['choices'][0]['message']['content']")" = "hello from gamma" ]
ck "chat model=gamma returns backend reply" $?
[ "$(state_of alpha)" = "stopped" ]; ck "alpha was evicted (same port lane)" $?
[ "$(state_of gamma)" = "ready" ]; ck "gamma is ready" $?

# 6. coexistence: alpha (21001) and beta (21002) at once
chat alpha >/dev/null
sleep 1
[ "$(state_of alpha)" = "ready" ]; ck "alpha back up" $?
chat beta >/dev/null
[ "$(state_of alpha)" = "ready" ] && [ "$(state_of beta)" = "ready" ]
ck "alpha and beta coexist (different ports)" $?

# 7. idle TTL reaping (ttl=3s)
echo "    waiting 6s for TTL reaper…"
sleep 6
[ "$(state_of alpha)" = "stopped" ] && [ "$(state_of beta)" = "stopped" ] && [ "$(state_of gamma)" = "stopped" ]
ck "idle entries reaped after TTL" $?

# 7b. external process holding an entry's port -> start must be refused
python3 - <<'PY' &
import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 21002))
s.listen(1)
time.sleep(15)
PY
OCC=$!
sleep 0.7
body=$(curl -s -X POST "$BASE/api/entries/beta/start")
echo "$body" | grep -q "already in use"; ck "external port occupant -> start refused" $?
kill "$OCC" 2>/dev/null
wait "$OCC" 2>/dev/null
sleep 0.3

# 8. embeddings endpoint routes by model
out=$(curl -s "$BASE/v1/embeddings" -H 'Content-Type: application/json' \
  -d '{"model":"beta","input":"hello"}')
echo "$out" | grep -q '"embedding"'; ck "POST /v1/embeddings model=beta" $?

# 9. generic passthrough /run/{id}/health
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/run/alpha/health")
[ "$code" = "200" ]; ck "GET /run/alpha/health -> 200" $?

# 10. multipart model extraction (ASR-style upload) routes by model field
out=$(curl -s "$BASE/v1/audio/transcriptions" -F model=alpha -F file=@/etc/hostname)
echo "$out" | grep -q "transcribed by alpha"; ck "multipart model field routes to alpha" $?

# 11. unknown model -> 404
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' -d '{"model":"nope","messages":[]}')
[ "$code" = "404" ]; ck "unknown model -> 404" $?

# 12. unmeasured GPU entries must not run together (first-run safety default)
chat delta >/dev/null
[ "$(state_of delta)" = "ready" ]; ck "gpu entry delta ready" $?
chat epsilon >/dev/null
{ [ "$(state_of epsilon)" = "ready" ] && [ "$(state_of delta)" != "ready" ]; }
ck "unmeasured GPU entries serialize (delta evicted for epsilon)" $?

echo
echo "-- P1: discovery / presets / entry management --"

# 12. discovery
scan=$(curl -s "$BASE/api/scan?dir=$(pwd)/testdata/scan")
echo "$scan" | grep -q '"candidates"'; ck "GET /api/scan returns candidates" $?
echo "$scan" | grep -q '22999'; ck "scan found fixture port 22999" $?
echo "$scan" | grep -q '"cpu"'; ck "scan guessed a CPU placement" $?

# 13. preset card
pre=$(curl -s "$BASE/api/entries/alpha/preset")
echo "$pre" | grep -q '"preset"'; ck "GET preset returns parsed script" $?

# 14. clone an entry
cl=$(curl -s -X POST "$BASE/api/entries" -H 'Content-Type: application/json' \
  -d '{"clone_of":"alpha","id":"alpha2","name":"Alpha 2"}')
echo "$cl" | grep -q '"created"'; ck "clone alpha -> alpha2" $?
curl -s "$BASE/v1/models" | grep -q '"alpha2"'; ck "cloned entry served in /v1/models" $?

# 15. rename (old id is kept as an alias)
rn=$(curl -s -X POST "$BASE/api/entries/alpha2/rename" -H 'Content-Type: application/json' \
  -d '{"id":"alpha3","name":"Alpha 3"}')
echo "$rn" | grep -q '"renamed"'; ck "rename alpha2 -> alpha3" $?
models=$(curl -s "$BASE/v1/models")
echo "$models" | grep -q '"alpha3"'; ck "renamed id served" $?
echo "$models" | grep -q '"alpha2"'; ck "old id kept as alias" $?

# 16. clone keeps the same port (same lane)
curl -s "$BASE/api/status" | python3 -c "
import json,sys
d=json.load(sys.stdin)
a=[e for e in d['entries'] if e['id']=='alpha3']
sys.exit(0 if a and a[0]['port']==21001 else 1)"; ck "clone kept the script port (lane)" $?

# 17. delete -> trash
dl=$(curl -s -X DELETE "$BASE/api/entries/alpha3")
echo "$dl" | grep -q '"trashed"'; ck "delete alpha3" $?
curl -s "$BASE/api/status" | grep -q '"alpha3"'; [ $? -ne 0 ]; ck "deleted entry gone from status" $?
ls "$STATE_DIR"/trash/ 2>/dev/null | grep -q alpha3; ck "deleted entry written to trash" $?

# 18. config stays valid after mutations
"$DECK" validate -config "$CONFIG" >/dev/null 2>&1; ck "config still valid after mutations" $?

# 19. status carries the host identity (hostname + hardware)
host=$(curl -s "$BASE/api/status" | python3 -c "
import json,sys
d=json.load(sys.stdin)
h=d.get('host') or {}
print(h.get('hostname',''))
")
[ -n "$host" ]; ck "status reports the host identity" $?

# 20. token mode: /api needs auth, the WebUI gets a session cookie
TOKPORT=12690
TOKCFG="$STATE_DIR/token-entries.yaml"
mkdir -p "$STATE_DIR"
sed "s|^    listen: .*|    listen: 127.0.0.1:$TOKPORT|; s|^    token: .*|    token: smoke-token|" \
  "$CONFIG" > "$TOKCFG"
"$DECK" serve -config "$TOKCFG" > "$STATE_DIR/token-deck.log" 2>&1 &
TOKDECK=$!
sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$TOKPORT/api/status")
[ "$code" = "401" ]; ck "token mode: /api/status without token -> 401" $?
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$TOKPORT/health")
[ "$code" = "200" ]; ck "token mode: /health stays open" $?
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer smoke-token' \
  "http://127.0.0.1:$TOKPORT/api/status")
[ "$code" = "200" ]; ck "token mode: bearer token accepted" $?
code=$(curl -s -c "$STATE_DIR/cookie.txt" -o /dev/null -w '%{http_code}' \
  "http://127.0.0.1:$TOKPORT/api/status?token=smoke-token")
[ "$code" = "200" ]; ck "token mode: ?token= accepted" $?
code=$(curl -s -b "$STATE_DIR/cookie.txt" -o /dev/null -w '%{http_code}' \
  "http://127.0.0.1:$TOKPORT/api/status")
[ "$code" = "200" ]; ck "token mode: session cookie works for the WebUI" $?
# opening the page with ?token= must hand the browser the cookie
code=$(curl -s -c "$STATE_DIR/cookie2.txt" -o /dev/null -w '%{http_code}' \
  "http://127.0.0.1:$TOKPORT/?token=smoke-token")
[ "$code" = "200" ]; ck "token mode: WebUI page accepts ?token=" $?
code=$(curl -s -b "$STATE_DIR/cookie2.txt" -o /dev/null -w '%{http_code}' \
  "http://127.0.0.1:$TOKPORT/api/status")
[ "$code" = "200" ]; ck "token mode: page cookie unlocks the dashboard" $?
kill "$TOKDECK" 2>/dev/null
wait "$TOKDECK" 2>/dev/null
sleep 0.3

# 21. multi-address listen: every address in the list answers
MULTIPORT=12692
MULTICFG="$STATE_DIR/multi-entries.yaml"
sed "s|^    listen: .*|    listen: 127.0.0.1:$MULTIPORT,127.0.0.1:$((MULTIPORT+1))|" \
  "$CONFIG" > "$MULTICFG"
"$DECK" serve -config "$MULTICFG" > "$STATE_DIR/multi-deck.log" 2>&1 &
MULTIDECK=$!
sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$MULTIPORT/health")
[ "$code" = "200" ]; ck "multi-listen: first address answers" $?
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$((MULTIPORT+1))/health")
[ "$code" = "200" ]; ck "multi-listen: second address answers" $?
kill "$MULTIDECK" 2>/dev/null
wait "$MULTIDECK" 2>/dev/null
sleep 0.3

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
