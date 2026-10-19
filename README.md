# modeldeck (`deck`)

A script-first, WebUI-managed scheduler and proxy for local model servers.

You already have hand-tuned startup scripts (`~/LLM/*.sh`, `~/scripts/LLM/**`, phone-side
scripts). modeldeck does **not** rewrite or translate them. It treats each script as the
source of truth, launches it on demand by model name, decides which scripts may run at the
same time, and releases RAM/VRAM after a configurable idle TTL.

It speaks an OpenAI-compatible API on one port (default `127.0.0.1:12688`), so existing
clients keep working; you just point `OPENAI_BASE_URL` at `deck` and pick a model name.

## Status

**P0 + early P1** — scheduler, data plane, control API, WebUI dashboard, first-run config
bootstrap (`deck init` / `start.sh`), and entry management (discovery, read-only presets,
rename/clone/trash with live config write-back). Validated by a 33-check smoke suite
(`testdata/smoke.sh`) and against a real backend (`~/LLM/jina-v5-embed-CPU.sh`). See
[`docs/PLAN.md`](docs/PLAN.md) for the full plan and roadmap (P1–P3).

## Quick start

```sh
git clone https://github.com/RoyChong5053/modeldeck.git
cd modeldeck
./update_build.sh                 # gofmt + vet + build ./deck

./start.sh                        # first run: creates the config, then serves
```

On the first run `start.sh` builds `./deck`, and if `~/.config/modeldeck/entries.yaml`
does not exist it creates it by scanning your script dirs (`deck init`: `~/LLM`,
`~/scripts/LLM/**`, …) and merging in every script it finds with a port. Then it runs
`deck serve` in the foreground; **Ctrl+C** stops deck and — by default — the models it
started (`KEEP=1 ./start.sh` leaves them running). No systemd required.

### Termux / Android

`deck` is a static Go binary (no CGO), so the same tree runs on a phone. On the phone:

```sh
pkg install golang          # once
cd ~/modeldeck              # source copied from the dev machine / git clone
./start-termux.sh           # build if needed, then serve in the foreground
```

`start-termux.sh` mirrors `start.sh` plus Android specifics:

| Command | What it does |
|---|---|
| `./start-termux.sh` | foreground; **Ctrl+C** stops deck and the models it started |
| `BG=1 ./start-termux.sh` | detached with `nohup`, log at `~/.local/state/modeldeck/deck.log` |
| `./start-termux.sh status` | health check + live entry table |
| `./start-termux.sh logs` | follow the background log |
| `./start-termux.sh stop` | stop deck (and its models) |
| `./start-termux.sh restart` | stop, then start in the background |

Environment knobs:

| Var | Default | Notes |
|---|---|---|
| `DECK_LISTEN` | `0.0.0.0:12688` | written into the config on start; use `127.0.0.1:12688` for local-only |
| `DECK_TOKEN` | empty | optional bearer token when exposed on the LAN |
| `MODELDECK_CONFIG` | `~/.config/modeldeck/entries.yaml` | config path |
| `KEEP=1` | off | leave started models running when deck exits |

On the first run it creates the config with `deck init` by scanning `~/LLM`. It also
takes a `termux-wake-lock` so Android does not freeze the process; `stop` releases it.
Keep the phone on the charger for long runs and exclude Termux from battery optimization.

Two phone-specific caveats:

- `deck init` cannot see ports passed through shell variables
  (`PORT="${1:-11438}"`, `PORT="${KOKO_PORT:-11435}"`), so it skips those scripts.
  Add the entries by hand (ASR: port 11438, ready `/health`; Kokoro TTS: port 11435,
  ready `/healthz`).
- Scanned scripts on a CPU-only phone are tagged `placement: gpu`; change them to
  `placement: cpu` so they can run together (the RAM budget still applies).

Do it by hand if you prefer:

```sh
./deck init            # or: ./deck scan -o ~/.config/modeldeck/entries.yaml
./deck scan -merge ~/.config/modeldeck/entries.yaml   # add newly written scripts later
# cp examples/entries.example.yaml ~/.config/modeldeck/entries.yaml
```

Open <http://127.0.0.1:12688> for the dashboard: start/stop/restart, live logs, resources,
script discovery (**Discover**), read-only **preset** cards, and **rename / clone / trash**.

Point a client at the same port:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:12688/v1
export OPENAI_API_KEY=modeldeck
curl -s $OPENAI_BASE_URL/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"gemma4","messages":[{"role":"user","content":"hi"}]}'
```

CLI:

```
deck serve [-config PATH] [-keep]   # -keep leaves models running on exit
deck init [-dir A,B] [-force]       # create a config by scanning your scripts
deck validate [-config PATH]
deck ls                             # live table from a running deck
deck scan [-dir A,B] [-o FILE | -merge FILE] [-force]
deck preset <id>                    # show an entry's parsed script preset
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

### Model-name rewriting and gated readiness

Clients often use a friendly model name that the backend itself rejects (for
example DeskSpirit sends `qwen3-asr`, while speech.cpp only serves
`Qwen3-ASR-0.6B`). Set `upstream_model` on the entry; deck routes on the client
name and rewrites the `model` field before proxying (JSON and multipart):

```yaml
- id: qwen3-asr
  aliases: [asr, whisper, Qwen3-ASR-0.6B]
  upstream_model: Qwen3-ASR-0.6B
  ...
```

Some servers answer `200` while their model is still loading (Kokoro TTS does).
Use `ready.pattern` to gate readiness on the response body:

```yaml
ready: { type: http, path: /healthz, pattern: '"status": "ok"', timeout: 600 }
```

Without the pattern, the first request after a cold start can hit a `503`.

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

`budgets.serialize_unknown_gpu` (default `true`) keeps two GPU entries whose memory has not
been measured from starting together, so a first run cannot over-commit a small card. Once a
model's peak is recorded, the normal VRAM budget takes over. Set it to `false` to opt out.

Each entry has an idle `ttl` (seconds, `0` = stay resident). After TTL with no in-flight
requests, deck runs an optional `stop.hook` (for servers with a self-unload endpoint), then
`SIGTERM` → grace → `SIGKILL` on the whole process group, then waits for the port (and
VRAM) to free before allowing a restart.

Processes are started in their own process group with `DECK_ENTRY`, `DECK_PORT`,
`DECK_URL`, `DECK_PID` injected into the environment. On startup, deck kills orphaned
children from a previous run (matched by `DECK_ENTRY`/`DECK_PID`).

## Managing variants

Model identity is the entry `id` (plus any `aliases`). Variants are short-lived, so all of
this is live — no restart, and the config file is rewritten atomically (a `.bak` is kept):

- **rename** (`改名`): changes the id and/or display name. The old id stays as an alias, so
  existing clients keep working.
- **clone** (克隆): copies an entry (same script/port/flags) under a new id.
- **trash** (删除): removes the entry from the config and writes it to
  `<state_dir>/trash/<timestamp>-<id>.yaml` before dropping it.
- **preset** (预设): read-only card showing the script's binary, model, port/host, placement
  and notable flags, parsed from the script itself.

The script is never rewritten: a rename changes only deck's labels, not the script file.

## Discovery

`deck scan` (or the **Discover** panel) walks your script directories (`~/LLM`,
`~/scripts/LLM/...`), parses each `*.sh`, and produces draft entries: it resolves shell
variables, reads `--port` / `--host`, identifies the launched binary and model, guesses
`cpu` vs `gpu` placement, and warns about missing model files or missing ports.

```sh
deck init                        # one-shot: scan default dirs and write a config
deck scan                       # list candidates
deck scan -o entries.yaml       # write a fresh config (refuses to overwrite without -force)
deck scan -merge ~/.config/…    # merge new scripts into an existing config
```

Duplicate ids across directories are disambiguated with a directory hint
(`gemma4-12b-un-agent-gpu`) or a numeric suffix.

## Control API

```
GET    /api/status                         # entries + budgets + mem + gpu
GET    /api/mem
GET    /api/entries
GET    /api/entries/{id}
POST   /api/entries                        # create; or {"clone_of": id, "id": new}
POST   /api/entries/{id}/rename            # {"id": new_id, "name": new_name}
DELETE /api/entries/{id}                   # move to state_dir/trash
GET    /api/scan?dir=A&dir=B               # discover scripts + draft entries
GET    /api/entries/{id}/preset            # read-only parsed preset
POST   /api/entries/{id}/start|stop|restart [?async=1]
POST   /api/entries/{id}/pin               {"pinned": true}
POST   /api/entries/{id}/ttl               {"ttl": 900}
GET    /api/entries/{id}/logs?tail=200
GET    /api/entries/{id}/logs/stream       # SSE
```

If `server.token` is set, `/api`, `/v1`, and `/run` require
`Authorization: Bearer <token>` (or `?token=`); `/health` and the WebUI stay open.

## Config

See `examples/entries.example.yaml`. Key fields:

```yaml
server:  { listen: 127.0.0.1:12688, token: "", state_dir: ~/.local/state/modeldeck }
budgets: { vram_reserve_mb, ram_reserve_mb, max_starting, on_no_fit: queue|reject,
           queue_timeout, ttl_default, serialize_unknown_gpu: true }
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
./deck serve -config testdata/entries.test.yaml &
bash testdata/smoke.sh     # 33 checks: routing, lanes, coexistence, TTL, port refusal,
                           # multipart, GPU-serialize, discovery, preset, clone, rename, trash
```

The smoke suite mutates the config, so run it against a throwaway copy (the helper
`run_smoke.sh` used during development does this automatically).

## Layout

```
cmd/deck            CLI entrypoint
internal/config     YAML schema, defaults, validation, atomic save
internal/registry   id/alias/port index (reloadable)
internal/runtime    process supervisor, readiness probes, logs, telemetry
internal/sched      three-layer scheduler + TTL reaper
internal/proxy      OpenAI data plane + /run passthrough + model extraction
internal/httpapi    control API + auth + SSE + entry management
internal/scan       script discovery / preset parsing
internal/web        embedded WebUI (plain JS)
start.sh            foreground launcher (Ctrl+C stops)
update_build.sh     gofmt + vet + build (+ optional git pull)
```
