# ModelDeck 部署知识（Deployment Notes）

> 这份文档记录**实际怎么部署、怎么连、怎么更新、踩过哪些坑**。
> 权威实施计划见 [`PLAN.md`](PLAN.md)，用法见 [`../README.md`](../README.md)。
> 文档里的地址/token 均为本机实际情况；**token 不要写进仓库**（仓库是公开的）。

---

## 1. 设备总览

| 设备 | ssh 别名 | 地址 | hostname | label | 平台 | 启动方式 | 仓库路径 |
|---|---|---|---|---|---|---|---|
| 主力机 | — | 192.168.10.1(网线) / 192.168.100.200(wifi) | cachyos-LOQ | 主力机 | Linux/systemd | **systemd 用户服务** | `~/mycode/modeldeck` |
| m64 | `m64` | 192.168.10.2(网线) / 192.168.100.78(wifi) | Dell-M6400 | m64 | Linux/systemd | systemd 用户服务 | `~/modeldeck` |
| d3180 | `d3180` | 192.168.100.38 | kali | d3180 | Linux/systemd | systemd 用户服务 | `~/modeldeck` |
| lgv35 | `lgv35` | 192.168.100.17 | LGV35 | lgv35 | Android/Termux | `start-termux.sh` | `~/modeldeck` |
| mate10 | `mate10` | 192.168.100.22 | ALP-L29 | mate10 | Android/Termux | `start-termux.sh` | `~/modeldeck` |
| gt20 | `gt20` | 192.168.100.242 | Infinix X6871 | gt20 | Android/Termux | `start-termux.sh` | `~/modeldeck` |

- ssh 别名在 `~/.ssh/config`（`m64` / `d3180` 是 22 端口；三台手机是 8022 端口的 Termux）。
- `dd3180` 是 d3180 的 root 通道（2222），日常用 `d3180` 即可。
- **主力机 ↔ m64 是网线直连**（192.168.10.0/24），不经过路由器；两边各有一个 wifi 地址走 192.168.100.0/24。
- Termux 的 `hostname` 永远是 `localhost`，deck 改用 `getprop ro.product.model` 区分（见 §8.4）。

---

## 2. 访问方式

### 2.1 WebUI

```sh
# 取本机 token
grep -E '^\s*token:' ~/.config/modeldeck/entries.yaml
```

| 设备 | WebUI |
|---|---|
| 主力机 | `http://127.0.0.1:12688/?token=<token>` 或 `http://192.168.10.1:12688/?token=<token>` |
| m64 | `http://192.168.10.2:12688/`（未设 token） |
| d3180 | `http://192.168.100.38:12688/`（未设 token） |
| lgv35 / mate10 / gt20 | `http://<各自 IP>:12688/`（未设 token） |

**token 怎么"绑定"？** 没有单独的绑定步骤：

1. token 写在配置 `server.token`，**改完要重启 deck 才生效**；
2. 浏览器打开 `http://<地址>:12688/?token=<token>` —— 服务端识别出 token 后，
   下发一个 `HttpOnly` 的 session cookie（`deck_session`，7 天）；
3. 之后直接开 `http://<地址>:12688/` 就能用，cookie 自动带上（面板右上角能看到主机名和色块）。

> 为什么要这一步：浏览器里的 `fetch` / `EventSource` 默认不带自定义头，
> 所以 WebUI 无法自己加 `Authorization: Bearer`。用 cookie 兜底，
> 前端一行代码都不用改。`/health` 和静态资源始终免鉴权。

### 2.2 API 客户端（one-api 等）

```
Base URL : http://192.168.10.1:12688        # 主力机
Key      : 与 server.token 相同（one-api 会发 Authorization: Bearer <key>）
模型名   : entry 的 id 或 alias（先 GET /v1/models 拿列表）
```

```sh
curl -s http://192.168.10.1:12688/v1/chat/completions \
  -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' \
  -d '{"model":"jina-rerank","messages":[{"role":"user","content":"hi"}]}'
```

⚠️ one-api 建渠道时会发一次 chat 做"测试渠道" → 会触发 30–60s 的模型冷启动，
one-api 那边可能超时。先用 `deck start <id>` 预热，再测渠道。

### 2.3 防火墙

主力机 ufw 已放行：`22 4000 8080 8888 11435 11436 11437 11438 11439 11440 12688 12888 …`

```sh
sudo ufw status verbose          # 查看
sudo ufw allow 12688/tcp         # 需要新增时
```

> 曾经"连不上"的根因不是防火墙，而是 deck 只绑了 `127.0.0.1`。
> **先看 `ss -tlnp | grep 12688` 确认监听地址**，再看防火墙。

---

## 3. 配置要点（`~/.config/modeldeck/entries.yaml`）

```yaml
server:
    listen: 127.0.0.1:12688,192.168.10.1:12688   # 可写多个地址，逗号分隔
    token: deck-<32 hex>                          # 空 = 不鉴权
    label: "主力机"                               # 显示在 WebUI 主机名后面
    state_dir: /home/roychong/.local/state/modeldeck
budgets:
    vram_reserve_mb: 800
    ram_reserve_mb: 2500
    max_starting: 2
    on_no_fit: queue          # queue=排队 | reject=直接 503
    queue_timeout: 180
    ttl_default: 300          # 空闲多久卸载模型
```

- `listen` 多个地址：**逗号分隔**，能绑上的都绑，绑不上的只告警不退出。
  主力机同时开 `127.0.0.1`（本机浏览器/CLI）和 `192.168.10.1`（m64 上的 one-api）。
- 改了 `listen`/`token`/`label` 要重启才生效（systemd: `systemctl --user restart modeldeck`）。
- `label` 是给人看的机器名；手机上 `hostname` 不可用，靠 `label` + getprop 型号区分。

---

## 4. 部署与更新

### 4.1 有 systemd 的机器（主力机 / m64 / d3180）

```sh
# 首次安装（或换仓库路径后重装）
cd <repo> && ./deploy/install-systemd.sh
sudo loginctl enable-linger $USER     # 退出登录后仍运行

# 日常更新
cd <repo> && git pull --ff-only && ./update_build.sh
systemctl --user restart modeldeck
```

`install-systemd.sh` 会按**当前 checkout 路径**生成 `~/.config/systemd/user/modeldeck.service`
（`WorkingDirectory`/`ExecStart` 指向当前目录），所以 `~/modeldeck` 和 `~/mycode/modeldeck` 都能用。

### 4.2 Termux（lgv35 / mate10 / gt20）

没有 systemd，继续用脚本：

```sh
cd ~/modeldeck && git pull --ff-only
./update_build.sh          # 需要 Termux 里装了 go（pkg install golang）
./start-termux.sh restart  # 停旧进程 + nohup 后台重启
./start-termux.sh status   # 健康检查 + entry 列表
./start-termux.sh logs     # 跟日志
```

`start-termux.sh` 负责：自动 `termux-wake-lock`、改写配置里的 `listen`/`token`、
后台启动、写 pidfile、就绪探测。

### 4.3 mate10 没有 Go：交叉编译

```sh
cd <repo> && ./deploy/build-android.sh mate10
```

脚本会编译 `GOOS=android GOARCH=arm64` 的二进制、scp 过去、重启。

> ⚠️ **必须 `GOOS=android`，不能用 `GOOS=linux`**。
> android/arm64 的 Go 运行时在第一次 `exec.Command` 时会探测 `pidfd_open`，
> Android 的 seccomp 直接用 **SIGSYS 杀进程** → deck 一启动就崩
> （日志里是 `SIGSYS: bad system call` + `internal/syscall/unix.PidFDOpen`）。

---

## 5. systemd 服务的运行细节

- 用户级 unit：`~/.config/systemd/user/modeldeck.service`，`Restart=always`，`RestartSec=2`。
- `loginctl show-user $USER -p Linger` 应为 `yes`，否则注销后服务会停。
- 日志：`journalctl --user -u modeldeck -f`。
- deck 退出时**默认会停掉它启动的模型**（`serve -keep` 可保留）。
  所以服务重启后模型是冷的，下次请求重新加载（30–60s）。
- 多个 deck 实例同机并存**不会互杀模型**（孤儿清理只杀"原属 deck 已死"的进程）。

---

## 6. 常用命令速查

```sh
# 本机
systemctl --user status modeldeck
journalctl --user -u modeldeck -f
ss -tlnp | grep 12688                     # 看实际监听地址
nvidia-smi --query-compute-apps=pid,used_memory --format=csv   # 谁占了显存
curl -s -H "Authorization: Bearer $(grep -oP 'token:\s*\K\S+' ~/.config/modeldeck/entries.yaml)" \
     http://192.168.10.1:12688/api/status | python3 -m json.tool | head -40

# 远端
ssh m64 'systemctl --user restart modeldeck; sleep 2; ~/modeldeck/deck ls'
ssh lgv35 'cd ~/modeldeck && ./start-termux.sh status'
```

---

## 7. 故障速查

| 现象 | 多半是 | 处理 |
|---|---|---|
| one-api 报连接失败 | deck 没绑对外地址 | `ss -tlnp \| grep 12688`；改 `listen` 加 `192.168.10.1` |
| WebUI 打开是"API 不可用" | 设了 token 但没带 cookie | 用 `?token=` 打开一次；或清掉 `deck_session` cookie 重来 |
| WebUI 表格挤成一团 | 旧版本前端 | 确认设备已到 v0.2.0+（`deck version`） |
| deck 起来就崩（手机） | 用了 linux/arm64 交叉编译 | 改用 `GOOS=android` 重新编译 |
| 模型启动失败 "port in use" | 有手工起的同名后端占着端口 | `ss -tlnp \| grep <port>` 找到并停掉 |
| 显存不够/OOM | 见 §8.1 | 调度器的账还没算对（待修） |
| 服务重启后模型全没了 | 正常行为 | 重新请求即冷启动；`serve -keep` 可保留模型 |

---

## 8. 踩过的坑（别再踩）

### 8.1 显存记账只算自己人
`budgetV = 总显存 − 预留`（8188−800=7388MB），但 deck 只统计**自己子进程**的显存。
桌面 ~1.1GB + 手工起的 llama-server（220+274MB）→ 真实可用只有 6764MB。
**手动起的模型不在 deck 管辖内**，会同时造成显存记账偏差和"端口已占用"。
→ 模型一律交给 deck 启停。

### 8.2 启动中的 entry 预留显存 = 0
L1 大模型加载要 30–60s，显存从 0 涨到 6GB，这段时间里别的 GPU entry 会被放进来 → OOM。

### 8.3 未知显存按 0 算
`serialize_unknown_gpu` 只互斥"未知 vs 未知"；已知大(6GB) + 未知(0MB) 直接放行 → OOM。

### 8.4 手机 hostname 都是 localhost
三台 Android 的 `hostname` 全是 `localhost`，浏览器里完全分不出谁是谁。
→ deck 主机名回退到 `getprop ro.product.model`；再配 `server.label`。

### 8.5 孤儿清理会误杀
旧逻辑：凡是带 `DECK_ENTRY`/`DECK_PID` 且不是自己起的进程就杀。
两个 deck 并存（测试实例 + 生产实例）时，后启动的会把先启动的模型全杀了。
→ 改成只杀"原属 deck 进程已死"的孤儿。

### 8.6 客户端超时 < 模型冷启动
llama-server 加载大模型要 30–60s，one-api / 客户端默认超时常常不够。
→ 先 `deck start <id>` 预热，或把客户端超时调到 120s+。

### 8.7 `deck init` 扫不出变量端口
`PORT="${1:-11438}"` 这种写法扫不出来 → 手工加 entry。

---

## 9. 待办（调度器内核）

审计结论见 [`PLAN.md` §10](PLAN.md)。要点：

- 真实 free 显存记账（替代 `total − reserve`）
- 启动中按悲观估值预留
- 未知显存按 tier 默认估值（L1 90% 卡 / L2 45% / L3 128MB），不再按 0
- 驱逐后等显存真的回落再启下一个
- L1/L2/L3 分级：L1 独占整卡、L2 可同时两个、L3（`-ngl 0 --no-kv-offload`，
  实测 **122MB**）永远挤得进来
- 真 FIFO 队列 + `/api/queue`
