# modeldeck (`deck`)

A script-first, WebUI-managed scheduler and proxy for local model servers.

You already have hand-tuned startup scripts (`~/LLM/*.sh`, `~/scripts/LLM/**`, phone-side
scripts). modeldeck does **not** rewrite or translate them. It treats each script as the
source of truth, launches it on demand by model name, decides which scripts may run at the
same time, and releases RAM/VRAM after a configurable idle TTL.

It speaks an OpenAI-compatible API on one port (default `0.0.0.0:12688`, all interfaces), so existing
clients keep working; you just point `OPENAI_BASE_URL` at `deck` and pick a model name.

**Deployment notes for the real fleet (6 devices, systemd + Termux, one-api in front)
live in [`docs/DEPLOY.md`](docs/DEPLOY.md).**

## Status

**P0 + early P1** — scheduler, data plane, control API, WebUI dashboard, first-run config
bootstrap (`deck init` / `start.sh`), and entry management (discovery, read-only presets,
rename/clone/trash with live config write-back). Validated by a 73-check smoke suite
(`testdata/smoke.sh`) and against a real backend (`~/LLM/jina-v5-embed-CPU.sh`). See
[`docs/PLAN.md`](docs/PLAN.md) for the full plan and roadmap (P1–P3).

The WebUI is built for phones first: the entries table collapses into cards on narrow
screens, and the **Events** + **Dashboard** panels are pinned to the top so the ongoing
operations and the hardware of the machine you are looking at are always visible. Every
device shows its own hostname (coloured chip + browser tab title), so five deck tabs are
told apart at a glance.

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

### Running as a service (Linux with systemd)

```sh
./deploy/install-systemd.sh       # writes ~/.config/systemd/user/modeldeck.service
sudo loginctl enable-linger $USER # keep it running after logout
journalctl --user -u modeldeck -f
```

The unit is generated from the current checkout, so it works from any directory
(`~/modeldeck`, `~/mycode/modeldeck`, …). Phones have no systemd — use
[`start-termux.sh`](#termux--android) there.

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
- Without a Go toolchain on the phone, cross-build with
  `./deploy/build-android.sh <host>` — the target **must** be `GOOS=android`
  (a `linux/arm64` build dies with SIGSYS as soon as deck spawns a model:
  Android's seccomp blocks the `pidfd_open` probe the Go runtime performs).

Do it by hand if you prefer:

```sh
./deck init            # or: ./deck scan -o ~/.config/modeldeck/entries.yaml
./deck scan -merge ~/.config/modeldeck/entries.yaml   # add newly written scripts later
# cp examples/entries.example.yaml ~/.config/modeldeck/entries.yaml
```

Open <http://127.0.0.1:12688> for the dashboard: start/stop/restart, live logs, resources,
script discovery (**Discover**), a plain-text **editor** for a model's YAML, **lock**, and
**rename / trash**. The deck binds every interface, so the same dashboard opens
from a phone on the LAN or over Tailscale — the startup banner prints the addresses.

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
deck token                          # prints the access token + one-api channel snippet
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
3. **Soft — preferences.** `priority` (lower evicted first), `pinned` (kept warm: not
   unloaded while idle, but released when a request needs the room), `evictable`
   (`false` = never evicted), `together_with`.

When a request needs memory that a *busy* entry holds, the request **waits** (FIFO queue
up to `queue_timeout`) instead of interrupting the in-flight request. Only idle, evictable
entries are evicted — a locked one only after the ordinary idle models are gone.
`on_no_fit: reject` returns `503` immediately instead.

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
this is live — no restart, and the config file is rewritten atomically (a `.bak` is kept).
Only the part that really changed is rewritten: hand-written comments in the config file
survive a save.

- **rename**: changes the id and/or display name. The old id stays as an alias, so
  existing clients keep working.
- **trash** (Delete): removes the entry from the config and writes it to
  `<state_dir>/trash/<timestamp>-<id>.yaml` before dropping it.
- **edit**: opens the YAML of that one entry in a plain text box — the same text that sits
  in the config file, so it is edited the way you would edit it with `nano`. A field name
  that does not exist is refused instead of being silently dropped, and a script path that
  does not exist is refused. *Save & run* applies it and restarts the model.
- **lock** (🔒): keeps the model *warm*. A locked entry is not unloaded while it is idle,
  so it does not have to be read from disk again. It is **not** a promise that the model
  stays in memory: when a request comes in and the VRAM/RAM does not fit, the locked
  model is released to make room — the request always wins over keeping a model warm. The
  flag is stored in the config and survives restarts. For a model that must never be
  dropped, use `evictable: false` in the config instead.
- **clone**: copies an entry (same script/port/flags) under a new id. It lives in the
  terminal, not in the WebUI: `deck clone <id> <new-id> [--name NAME]`. The copy shares
  the port with the original, so only one of the two runs at a time.

The script file is never rewritten: these actions change deck's own config, not the
launcher on disk.

### Editing the config by hand

The config file is a normal YAML file and deck reads it back on demand:

```sh
nano ~/.config/modeldeck/entries.yaml     # edit it
```

Press **Reload** in the WebUI (or `curl -X POST /api/config/reload`) and the running deck
picks the file up — the models that are running keep running, only entries that vanished
from the file are stopped. A file that does not parse is refused with the YAML error, and
the deck keeps the configuration it had.

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
GET    /api/status                         # entries + budgets + mem + gpu + host
GET    /api/mem
GET    /api/entries
GET    /api/entries/{id}
POST   /api/entries                        # create; or {"clone_of": id, "id": new}
POST   /api/entries/{id}/rename            # {"id": new_id, "name": new_name}
DELETE /api/entries/{id}                   # move to state_dir/trash
GET    /api/scan?dir=A&dir=B               # discover scripts + draft entries
GET    /api/entries/{id}/config            # editable config + parsed script info
POST   /api/entries/{id}/config            # edit script/port/args/env/ttl/ready_path
GET    /api/entries/{id}/yaml              # the entry as YAML text (the editor)
POST   /api/entries/{id}/yaml              # {"yaml": "..."} — replace the entry
GET    /api/config                         # the whole config file as text
POST   /api/config/reload                  # re-read the config file from disk
POST   /api/entries/{id}/start|stop|restart [?async=1]
POST   /api/entries/{id}/lock              {"locked": true}   # keep warm (idle-proof)
POST   /api/entries/{id}/reset             # clear a stale "failed" state
POST   /api/entries/{id}/ttl               {"ttl": 900}
GET    /api/entries/{id}/logs?tail=200
GET    /api/entries/{id}/logs/stream       # SSE
```

`/api/status` also returns a `host` block — hostname, optional `server.label`, deck
version, uptime, live GPU/CPU/RAM figures, and `host.urls`: the addresses other machines
can open (with a wildcard bind: every local address, LAN and Tailscale alike). The WebUI
uses it for the Dashboard panel and to label the tab with the machine it talks to.

If `server.token` is set, `/api`, `/v1`, and `/run` require
`Authorization: Bearer <token>` (or `?token=`); `/health` and the WebUI stay open.
Opening the WebUI as `http://<host>:12688/?token=<token>` hands the browser a session
cookie, so the dashboard keeps working without any client-side change.

## Config

See `examples/entries.example.yaml`. Key fields:

```yaml
server:  { listen: 0.0.0.0:12688, token: "", label: "", state_dir: ~/.local/state/modeldeck }
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
bash testdata/smoke.sh     # 73 checks: routing, lanes, coexistence, TTL, port refusal,
                           # multipart, GPU-serialize, discovery, preset, clone, rename, trash,
                           # lock (keeps warm, and yields to a request), YAML editor, reload
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
internal/version    build version
internal/web        embedded WebUI (plain JS)
deploy/             systemd user unit + installer
start.sh            foreground launcher (Ctrl+C stops)
update_build.sh     gofmt + vet + build (+ optional git pull)
```
