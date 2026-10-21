# ModelDeck 实施计划

> 本文件是唯一权威计划，位于仓库 `docs/PLAN.md`。若上下文被压缩，先读本文件，再读「进度日志」与「下一步」两节继续推进。
> 代码仓库：https://github.com/RoyChong5053/modeldeck ，本地 `/home/roychong/mycode/modeldeck`，二进制名 `deck`。

## 0. 进度日志（每次推进后更新）

- [x] 决策收敛（见「已锁定决策」）
- [x] 计划文档落盘
- [x] 部署知识文档：`docs/DEPLOY.md`（设备总览/访问方式/更新流程/坑位速查）
- [x] P0-1 脚手架 + config + registry
- [x] P0-2 runtime supervisor（启动/就绪/停止/日志/遥测）
- [x] P0-3 scheduler（三层约束 + 排队）
- [x] P0-4 proxy（OpenAI 入口 + /run + /v1/models）
- [x] P0-5 control API + WebUI Dashboard
- [x] P0-6 CLI
- [x] P0-7 冒烟测试（fake server）全绿：17/17
  （路由/别名/同端口 lane 驱逐/CPU 共存/TTL 回收/外部占端口拒绝/embeddings/multipart/`/run`）
- [x] P0-8 真机验证（`~/LLM/jina-v5-embed-CPU.sh`，CPU embed）
  - 冷启动 1.3s，OpenAI `/v1/embeddings` 返回 768 维；RSS 实测回填 133MB，VRAM 0
  - TTL=20s 后自动停止，端口 11436 释放，日志落盘 `<state_dir>/logs/<id>.log`
- [x] P1-a 脚本扫描导入：`deck scan` + `GET /api/scan`（解析变量/`--port`/`--host`/二进制/模型/placement，缺端口或模型给告警）
- [x] P1-b 只读预设卡片：`deck preset` + `GET /api/entries/{id}/preset` + WebUI 弹窗
- [x] P1-c 改名/克隆/回收站：配置原子写回（`.bak`）+ 热重载（`reg.Rebuild` + `sup.SyncRuntimes`），无需重启
- [x] P1-d 实测资源写回：entry 管理保存配置时一并持久化（独立定时写回仍留待 P2）
- [x] 便利脚本：`start.sh`（前台运行，Ctrl+C 即停；`KEEP=1` 保留模型）、`update_build.sh`（gofmt+vet+build，`--pull` 可选）
- [x] 冒烟测试扩展到 33/33（新增 discovery/preset/clone/rename/trash + 未测显存 GPU 串行；用临时配置副本，避免污染仓库）
- [x] 首次运行体验：`deck init`（扫描默认目录生成配置；重名 id 用目录提示去重）、`start.sh` 缺配置时自动 init；`deck serve` 缺配置时给出明确指引
- [x] 首跑安全：`budgets.serialize_unknown_gpu`（默认 true）——实测显存未知的两个 GPU entry 不并发，避免 8GB 卡首次并发 OOM
- [x] P1-e 预设参数表单写回脚本 → 归入 P2
- [x] P2 预设表单写回脚本 + server-selfunload + 资源历史
- [x] P3 job 模式 + 旧端口 shim + 多设备 agent
- [x] WebUI 移动端重构 + 置顶 Events/Dashboard + 主机身份（hostname/label/硬件状态）
- [x] 鉴权补洞：设了 `server.token` 时 WebUI 仍可用（`?token=` → session cookie）
- [x] systemd 用户级服务：`deploy/install-systemd.sh`（按当前 checkout 生成 unit）
- [ ] 调度器 P0 安全修复（真实 free 显存 / 悲观未知 / 启动预留 / 驱逐后等显存）
- [ ] L1/L2/L3 资源分级模型（tier + 并发上限 + 独占 + 物理余量）
- [ ] FIFO 排队队列 + `/api/queue` 可观测

### WebUI 验证

- 浏览器渲染 OK（Dashboard/Entries/Budgets/Events）；入口 `/`、`/static/*` 正常。
- 表格改为 keyed 原地更新（不再每 2s 重建 DOM），UI 点“启动”可正确拉起 entry。
- 修过的两个 bug：嵌入静态资源未剥 `/static` 前缀（404）；行按钮闭包读到未赋值的 `refs.id`。
- 2026-10-21 移动端重构：窄屏下表格变卡片（`data-label` + `table.responsive`）；
  Events（进行中运作）与 Dashboard（主机/硬件/预算）置顶并可折叠；`/api/status` 增加
  `host` 块（hostname / label / 版本 / uptime / GPU / CPU / RAM），页签标题与主机色块用于
  区分多台设备；手机端 Dashboard 退化为一行可横向滚动的状态条。

### P1 落地说明

- 热重载路径：`commitEntries` → 校验 → `config.Save`（临时文件 + rename，保留 `.bak`）→ `registry.Rebuild` → `supervisor.SyncRuntimes`（消失的 entry 先停后删，其余刷新 Entry 指针）。全程无需重启；代理与调度器读的是同一份 cfg 指针与可重载 registry。
- 改名保留旧 id 到 `aliases`；**不改脚本文件**（变体身份只是 deck 的标签，脚本参数才是资产）。
- 删除会把 entry YAML 写到 `<state_dir>/trash/<时间戳>-<id>.yaml` 再移除。
- 克隆保留原脚本与端口（同一 lane），仅改 id/name。
- 扫描启发式：shell 变量替换（`$VAR`/`${VAR}`）→ `exec` 取二进制 → `--port`/`--host`（含 JSON 形式）→ 模型路径 → `-ngl`/`--embeddings` 等 flag → 路径含 `cpu` 或 `-ngl 0` 判 CPU。模型文件不存在、缺端口都会给告警。
- WebUI 新增 Discover 面板（扫描 + 一键导入）与每行 `预设/改名/克隆/删除`；表格为 keyed 原地更新。
- 首跑体验：`start.sh` 构建 → 缺配置则 `deck init` 扫描 `~/LLM` 等目录生成 `entries.yaml` → 前台 `serve`；`deck serve` 在配置缺失时直接打印 `deck init`/`deck scan -o`/示例三条指引。
- 首跑安全：`serialize_unknown_gpu` 默认开启，实测显存未知的 GPU entry 之间互斥（`planLocked` 里作为 hard victim），首次跑完后按实测预算正常并发。

### P0 落地说明（与计划的偏差）

- 资源回填目前仅在内存中生效（`resources` 峰值为进程生命周期内最大值）；写回 YAML 留到 P1/P2。
- 停止后仅等待端口释放（`waitPortFree`）；VRAM 回落轮询留待 P2（已记录）。
- `together_with` 目前只保证“不被 E 驱逐”，未做双向联动。
- WebUI 的 Budgets 面板 P0 只读（与计划一致）。


## 1. 目标与定位

脚本优先的本地模型调度台：

- 把 `~/LLM/*.sh`、`~/scripts/LLM/**/*.sh` 当作一等公民；脚本是 source of truth，deck 不改写脚本。
- 数据面模仿 llama-swap：OpenAI 兼容入口，按 `model`（变体 id/别名）路由。
- 核心是**可 UI 配置的共存/资源调度器**：决定哪些脚本能同时跑，空闲按 TTL 优雅释放内存。
- 默认只监听 `127.0.0.1:12688`，需要局域网时改 `0.0.0.0` + token。

## 2. 已锁定决策

| 项 | 决定 |
|---|---|
| 模型标识 | 变体 id/别名；改名安全（旧 id 进 aliases）；可克隆/删除/回收站 |
| 入口端口 | 12688；默认 bind 127.0.0.1 |
| 并发模型 | 不是简单 lane 互斥，而是三层约束：端口唯一（硬）+ VRAM/RAM 预算（实测）+ together/mutex/priority/pin（软） |
| 预算不足 | **排队等空闲**（FIFO + 超时），绝不中断正在服务的请求 |
| 资源数值 | 首次运行实测峰值 + 手动覆盖（`manual_resources: true` 时不自动写回） |
| 释放方式 | 优雅 SIGTERM + 可选 stop hook + 等待端口/VRAM 释放；P2 加 server-selfunload |
| 端口被外部占用 | 拒绝启动并在 UI/接口提示，不盲杀 |
| 预设编辑 | P0 不做；P1 只读卡片；P2 表单写回 |
| job（CLI）模式 | P3 |
| MVP | 调度器 + Dashboard/起停/日志 + Resources 页 |
| 项目路径 | `/home/roychong/mycode/modeldeck` |
| 技术栈 | Go 单二进制 + 内嵌 WebUI；纯 Go 无 cgo；amd64/arm64 |

## 3. 架构与目录结构

```
mycode/modeldeck/
  go.mod
  cmd/deck/main.go              # CLI: serve/validate/ls/start/stop/restart/env
  internal/config/              # YAML schema + 加载/校验/默认值
  internal/registry/            # id/alias/port 索引
  internal/runtime/             # 进程组 supervisor、就绪探测、日志环形缓冲、遥测
  internal/sched/               # 三层约束调度器、TTL、排队
  internal/proxy/               # 反向代理 + model 解析 + /run + /v1/models
  internal/httpapi/             # 控制面 API + SSE + 鉴权中间件
  internal/web/                 # //go:embed 静态页面
    static/{index.html,app.js,style.css}
  testdata/fake_server.py       # 冒烟测试用后端
  testdata/entries.test.yaml
  examples/entries.example.yaml
  README.md
```

依赖：仅 `gopkg.in/yaml.v3`（配置）。其余用标准库（`net/http`、`httputil.ReverseProxy`、`os/exec`、`syscall`）。

## 4. Entry schema

```yaml
version: 1
server:
  listen: 127.0.0.1:12688
  token: ""
  state_dir: ~/.local/state/modeldeck   # 日志/运行态
  scripts_dir: ~/LLM
budgets:
  vram_reserve_mb: 800
  ram_reserve_mb: 2500
  max_starting: 2
  on_no_fit: queue        # queue | reject
  queue_timeout: 120      # 秒
  ttl_default: 300        # 秒
entries:
  - id: gemma4-12b-agent
    name: "Gemma4-12B agent"
    aliases: [agent, 12b-agent]
    script: ~/LLM/gemma4-12b-agent.sh
    args: []              # 追加参数
    env: []               # KEY=value
    cwd: ""
    mode: server          # server | job(P3)
    host: 127.0.0.1       # 后端监听地址（与本项 port 一起用于代理与健康检查）
    port: 12888
    ready: { type: http, path: /health, timeout: 600 }
    ttl: 900              # 0=常驻；省略=budgets.ttl_default
    placement: gpu        # gpu | cpu | hybrid
    resources: { vram_mb: 0, ram_mb: 0 }   # 0=未实测，首跑后自动回填
    manual_resources: false
    exclusive: false
    evictable: true
    pinned: false
    priority: 10
    together_with: []
    mutex_with: []
    stop: { grace_s: 10, hook: "" }
    tags: [rp, gpu, gemma4]
```

`ready.type`: `http` | `tcp` | `log` | `sleep` | `none`。

deck 启动子进程时注入环境变量：`DECK_ENTRY=<id>`、`DECK_PORT=<port>`、`DECK_URL=http://<host>:<port>`、`DECK_PID=<supervisor pid>`。脚本可以忽略它们（多数写死端口）。

## 5. 调度器算法（三层约束）

状态：`running`（supervisor 中 ready/starting）+ `reserved`（正在启动/正在停止的占位）。

```
Acquire(ctx, E):
  loop:
    lock
    if E 已 ready: inflight++; unlock; return
    if E 正在 starting: wait(waitCh, ctx); continue
    victims, ok, reason := plan(E)
    if !ok:
        if on_no_fit == reject: unlock; return 503(reason)
        wait(waitCh | ctx | queue_timeout); continue
    reserve(E as starting)
    for v in victims: reserve(v as stopping)
    unlock
    for v in victims: sup.Stop(v); lock; unreserve(v); broadcast(); unlock
    rt, err := sup.Start(ctx, E)         # 阻塞到 ready
    lock; unreserve(E); broadcast(); unlock
    if err: return err
    rt.inflight++; return rt

plan(E):
  candidates/usage = running + reserved
  victims = {}
  # 硬约束
  if E.port 被 F 占用: victims += F
  victims += {F : mutex(E,F)}
  if E.exclusive: victims += all others
  # 若有硬约束 victim 处于 starting/忙 → 不能排出 → !ok
  # 资源约束
  while projected_usage(victims) + E.resources > budget:
      F = running 中 idle(inflight==0) 且 evictable 且 !pinned
          且 F ∉ together_with(E) 且 F ∉ victims
          中「priority 最低，其次 lastUsed 最旧」者
      if 无: return !ok("资源不足且无空闲可驱逐")
      victims += F
  if starting_count + 1 > max_starting: return !ok("启动并发已达上限")
  return victims, ok
```

预算：`projected = Σ(vram/ram of running+reserved-except-victims) + E`，与 `budget = total - reserve` 比较；拿不到总量时用 `/proc/meminfo` 的 `MemAvailable` 与 `nvidia-smi` 空闲值，退化为"只剩 reserve"。

TTL 巡检（1s tick）：`ready && inflight==0 && !pinned && ttl>0 && now-lastUsed>ttl` → 走同一条 Stop 路径并 broadcast。

停止：可选 `stop.hook`（先跑 hook 调服务端 unload）→ 否则 `SIGTERM` 整个进程组 → 等 `grace_s` → `SIGKILL` 进程组 → `wait` → 轮询直到端口可再次 bind（最多 5s）→ 采样 VRAM 直到回落或超时。

孤儿清理：启动时扫 `/proc/*/environ`，含 `DECK_ENTRY=` 且 `DECK_PID != 本进程` 的进程，kill 进程组。

## 6. 数据面 / 管理面

数据面（12688）：
- `POST /v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/rerank`、`/v1/audio/speech`、`/v1/audio/transcriptions`
- `GET /v1/models`（聚合所有 entry，含 aliases 可选）
- `ANY /run/{id}/{path...}`（非标准服务通用入口，剥前缀后转发）
- `GET /health`

model 解析顺序：`X-Deck-Model` 头 → `?model=` → JSON body 的 `model` → multipart 表单字段 `model`（大 body 落临时文件）→ 失败 400。

管理面：
- `GET /api/status`、`GET /api/entries`、`GET /api/mem`
- `POST /api/entries/{id}/start|stop|restart`
- `POST /api/entries/{id}/pin`（body `{"pinned":true}`）
- `POST /api/entries/{id}/ttl`（body `{"ttl":900}`，运行中生效）
- `GET /api/entries/{id}/logs?tail=200`、`GET /api/entries/{id}/logs/stream`（SSE）
- 鉴权：`server.token` 非空时 `/api`、`/v1`、`/run` 需要 `Authorization: Bearer <token>`；`/health` 与静态资源放行。

## 7. WebUI（P0）

- Dashboard：条目表格（name/state/port/inflight/RSS/VRAM/TTL 倒计时/错误）、start/stop/restart/pin、日志弹窗（SSE）、内存可用条。
- Resources：预算展示（P0 只读，P1 可编辑）、每 entry 声明 vs 实测资源、placement/tags。
- 无构建步骤：原生 JS + fetch + EventSource。

## 8. 里程碑与验收

- P0 验收：fake server 冒烟全绿；PC 上接 2~3 个真实脚本能按 model 名切换、TTL 后内存回落；端口占用会被拒绝并提示。
- P1：扫描目录自动预填；预设只读卡片；改名/克隆/回收站。
- P2：预设表单写回（备份 + `zsh -n`）；server-selfunload；资源历史曲线。
- P3：job 模式；旧端口 shim；多设备 agent（d3180 Kokoro）。

## 9. 风险与对策

- VRAM 释放延迟 → 停止后轮询 `nvidia-smi` 回落再启动。
- 脚本后台化脱组 → 启动时检测脚本是否 `exec`，警告。
- 脚本写死 `--host 0.0.0.0` → 后端仍暴露 LAN（现状），后续加 host 覆盖。
- Android LMK 杀 supervisor → wake lock + `oom_score_adj` 包装 + 孤儿清理。
- 冷启动 20–60s → 客户端超时要够；UI pin 常驻。
- `sync.Cond` 不感知 ctx → 用 waitCh 广播 + select(ctx/timer)。

## 10. 下一步（恢复上下文后从这里继续）

P0/P1 全部完成；WebUI 移动端重构 + 主机身份 + systemd 部署已落地，冒烟 39/39。

### 10.1 调度器 P0 安全修复（治本机 8GB 卡 OOM 风险）

审计发现（2026-10-21，本机 RTX 4060 8188MB）：

1. **预算看不见别人占的显存**：`budgetV = total - reserve`（8188-800=7388），但 deck 只统计
   自己子进程；桌面 ~1.1GB + 手工起的 llama-server（220+274MB）→ 真实可用仅 6764MB。
   → 改为用 nvidia-smi 的**真实 free**，决策时现读。
2. **启动中的 entry 预留显存 = 0**：L1 加载 30–60s 期间显存从 0 涨到 6GB，别的 GPU entry
   会被放进来。→ `starting` 按 `need(E)` 悲观记账。
3. **未知显存按 0 算**：`serialize_unknown_gpu` 只互斥「未知 vs 未知」；已知大 + 未知 = 放行。
   → 未知按 tier 默认估值（L1 90% 卡 / L2 45% / L3 128MB）。
4. **驱逐后不等显存释放**：`Stop` 只等端口。→ 轮询 nvidia-smi 确认回落再启下一个。

### 10.2 L1/L2/L3 资源分级模型

```yaml
budgets:
  gpu: { reserve_mb: 256 }
  tiers:
    L1: { max_concurrent: 1, exclusive_with: [L1, L2] }   # 独占整卡
    L2: { max_concurrent: 2 }                              # 半卡，可同时两个
    L3: { max_concurrent: 8, tiny: true }                  # 几十~几百MB，永远挤得进来
entries:
  - id: jina-rerank-no-vram-gpu
    tier: L3          # 留空则自动推导
```

- **自动推导**：`placement: cpu` 不参与 GPU 分级；有实测峰值 → ≥60% 卡=L1 / ≥20%=L2 / 否则 L3；
  无实测且脚本含 `-ngl 0` / `--no-kv-offload` → L3；否则 L1（悲观）。
- **三条硬规则**：物理够放（真实 free − need ≥ reserve）→ 同 tier 并发 < max → 独占关系
  （仅 L1↔L1/L2；L3 永不因级别被驱逐）。
- 实测基线（本机）：`-ngl 0 --no-kv-offload` = 122MB；`-ngl 99` 的 jina = 220/274MB。

### 10.3 FIFO 队列

现在排队是 `waitCh` 广播唤醒（先来后到不保证）→ 改成每个 entry 一个 FIFO waiter 队列，
并暴露 `GET /api/queue`（等待者、位次、等待时长）。

### 10.4 其余

- 预设**表单编辑写回脚本**（备份 + `zsh -n`）；`server-selfunload`；资源历史曲线。
- 真机再补 GPU chat（12888）+ CPU rerank（11437），验证 VRAM 预算驱逐与 CPU/GPU 共存。
- job 模式、旧端口 shim、多设备 agent（d3180 Kokoro / lgv35）。

回归方式：`./update_build.sh` 后用临时配置副本跑
`./deck serve -config <临时配置> &` + `bash testdata/smoke.sh`（当前 39/39）。
