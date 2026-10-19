# modeldeck (`deck`)

A script-first, WebUI-managed scheduler and proxy for local model servers.

You already have hand-tuned startup scripts (`~/LLM/*.sh`, `~/scripts/LLM/**`, phone-side
scripts). modeldeck does **not** rewrite or translate them. It treats each script as the
source of truth, launches it on demand by model name, decides which scripts may run at the
same time, and releases RAM/VRAM after a configurable idle TTL.

It speaks an OpenAI-compatible API on one port (default `127.0.0.1:12688`), so existing
clients keep working; you just point `OPENAI_BASE_URL` at `deck` and pick a model name.

## Status

**P0 complete** — scheduler + data plane + control API + WebUI dashboard. Validated by a
17-check smoke suite (`testdata/smoke.sh`) and against a real backend
(`~/LLM/jina-v5-embed-CPU.sh`). See [`docs/PLAN.md`](docs/PLAN.md) for the full plan
and roadmap (P1–P3).

## Quick start

```sh
cd ~/mycode/modeldeck
go build -o deck ./cmd/deck

cp examples/entries.example.yaml ~/.config/modeldeck/entries.yaml
$EDITOR ~/.config/modeldeck/entries.yaml      # point at your scripts and ports

./deck validate                                # parse + sanity check
./deck serve                                   # scheduler + proxy + WebUI
```

Open <http://127.0.0.1:12688> for the dashboard (start/stop/restart/pin, live logs,
resources). Point a client at the same port:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:12688/v1
export OPENAI_API_KEY=modeldeck
curl -s $OPENAI_BASE_URL/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"gemma4","messages":[{"role":"user","content":"hi"}]}'
```

CLI:

```
deck serve [-config PATH] [-keep]   # -keep leaves models running on exit
deck validate [-config PATH]
deck ls                             # live table from a running deck
deck start|stop|restart <id>
deck env <id>                       # prints client env + curl example
```

## How routing works

The model name in the request (an entry `id` or any of its `aliases`) selects the entry.
The name is read from, in order: `X-Deck-Model` header, `?model=`, the JSON `model`
field, or the multipart `model` field (large uploads are spooled to a temp file).

Endpoints:

| Path | Notes |
|---|---|
| `POST /v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/messages` | chat |
| `POST /v1/embeddings`, `/v1/rerank`, `/rerank` | embed / rerank |
| `POST /v1/audio/speech`, `/v1/audio/transcriptions` | TTS / ASR |
| `GET /v1/models` | aggregated ids + aliases |
| `ANY /run/{id}/{path...}` | generic passthrough for non-standard servers (prefix stripped) |

## The scheduler

Three layers, all UI/API-configurable:

1. **Hard — port uniqueness (lane).** Entries that share a port can never run together.
   An entry can also declare `exclusive: true` (runs alone) or `mutex_with: [ids]`.
2. **Resource — measured budgets.** VRAM is sampled via `nvidia-smi`, RAM via
   `/proc`. Peaks measured on the first run feed back into `resources` (unless
   `manual_resources: true`). `budgets.*_reserve_mb` is kept free.
3. **Soft — preferences.** `priority` (lower evicted first), `pinned` (never evicted),
   `evictable`, `together_with`.

When a request needs memory that a *busy* entry holds, the request **waits** (FIFO queue
up to `queue_timeout`) instead of interrupting the in-flight request. Only idle, evictable,
non-pinned entries are evicted. `on_no_fit: reject` returns `503` immediately instead.

Each entry has an idle `ttl` (seconds, `0` = stay resident). After TTL with no in-flight
requests, deck runs an optional `stop.hook` (for servers with a self-unload endpoint), then
`SIGTERM` → grace → `SIGKILL` on the whole process group, then waits for the port (and
VRAM) to free before allowing a restart.

Processes are started in their own process group with `DECK_ENTRY`, `DECK_PORT`,
`DECK_URL`, `DECK_PID` injected into the environment. On startup, deck kills orphaned
children from a previous run (matched by `DECK_ENTRY`/`DECK_PID`).

## Control API

```
GET  /api/status                         # entries + budgets + mem + gpu
GET  /api/mem
GET  /api/entries
GET  /api/entries/{id}
POST /api/entries/{id}/start|stop|restart [?async=1]
POST /api/entries/{id}/pin               {"pinned": true}
POST /api/entries/{id}/ttl               {"ttl": 900}
GET  /api/entries/{id}/logs?tail=200
GET  /api/entries/{id}/logs/stream       # SSE
```

If `server.token` is set, `/api`, `/v1`, and `/run` require
`Authorization: Bearer <token>` (or `?token=`); `/health` and the WebUI stay open.

## Config

See `examples/entries.example.yaml`. Key fields:

```yaml
server:  { listen: 127.0.0.1:12688, token: "", state_dir: ~/.local/state/modeldeck }
budgets: { vram_reserve_mb, ram_reserve_mb, max_starting, on_no_fit: queue|reject,
           queue_timeout, ttl_default }
entries:
  - id, name, aliases, script, args, env, cwd
    mode: server|job            # job is reserved for P3
    host, port                  # the script's own bind address
    ready: { type: http|tcp|log|sleep|none, path, pattern, seconds, timeout }
    ttl
    placement: gpu|cpu|hybrid
    resources: { vram_mb, ram_mb }, manual_resources
    exclusive, evictable, pinned, priority, together_with, mutex_with, tags
    stop: { grace_s, hook }
```

## Tests

```sh
./deck validate -config testdata/entries.test.yaml
./deck serve -config testdata/entries.test.yaml &
bash testdata/smoke.sh     # 17 checks: routing, lanes, coexistence, TTL, port refusal, multipart
```

## Layout

```
cmd/deck            CLI entrypoint
internal/config     YAML schema, defaults, validation
internal/registry   id/alias/port index
internal/runtime    process supervisor, readiness probes, logs, telemetry
internal/sched      three-layer scheduler + TTL reaper
internal/proxy      OpenAI data plane + /run passthrough + model extraction
internal/httpapi    control API + auth + SSE
internal/web        embedded WebUI (plain JS)
```
