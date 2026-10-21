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

### 2.1 令牌放在哪：每台设备项目目录下的 `.env`

**每台设备各有一份,就在 modeldeck 项目文件夹里**(不是 home):

| 设备 | 令牌文件 |
|---|---|
| 主力机 | `~/mycode/modeldeck/.env` |
| m64 / d3180 | `~/modeldeck/.env` |
| lgv35 / mate10 / gt20 | `~/modeldeck/.env` |

```
# modeldeck access token — this device
# 复制这个文件到新设备即可复用同一令牌；one-api 渠道的「密钥」填 DECK_TOKEN 的值。
DECK_TOKEN=deck-515c6733b75278fd81a61a3467598ae3
```

- **换新设备**：把这个 `.env` 拷过去就行(或者 `scp m64:~/modeldeck/.env .`)。
- systemd 设备：unit 里有 `EnvironmentFile=<项目>/.env` → 令牌自动进环境变量。
- Termux 设备：`start-termux.sh` 从 `.env` 导出 `DECK_TOKEN`，**跑完就打印令牌**。

```sh
cat ~/modeldeck/.env                          # 查
./start-termux.sh token                       # Termux：查（含 one-api 渠道配置）
cd ~/mycode/modeldeck && ./deck token          # 任意平台：查
cd ~/mycode/modeldeck && ./deck token --write  # 补建 .env（没有就生成一个）
```

> `.env` 已在 `.gitignore` 里，仓库只留 `.env.example`。
> 服务端只认「YAML 里的 token → `$DECK_TOKEN`」，**不会自己去翻 `.env`**：
> 否则同机跑第二个 deck 实例会被莫名其妙要求鉴权（踩过：冒烟 40 项全挂）。
> `.env` → 环境变量这一步由部署层做（systemd EnvironmentFile / 脚本 export）。

### 2.2 WebUI 怎么进

**deck 现在监听 `0.0.0.0:12688`（所有网卡）** —— 不再绑死某个 IP：

- DHCP 换了地址照样能开；
- Tailscale 的 `100.x` 地址自动可用（m64 上有 `tailscale0`，gt20 通过 Tailscale 连 m64 就走这个地址）；
- 启动横幅会把**本机所有可用地址**都列出来，照抄即可。

```sh
# 取本机 token
grep -oP 'DECK_TOKEN=\K\S+' ~/modeldeck/.env
```

| 设备 | WebUI 地址 |
|---|---|
| 主力机 | `http://192.168.10.1:12688/`（有线，直连 m64）或 `http://192.168.100.200:12688/`（WiFi，手机走这个） |
| m64 | `http://192.168.10.2:12688/`、`http://192.168.100.78:12688/`、Tailscale `http://100.116.75.51:12688/` |
| d3180 | `http://192.168.100.38:12688/` |
| lgv35 / mate10 / gt20 | `http://<各自 192.168.100.x>:12688/` |

> 不确定当前地址？`./deck token` 会列出全部；WebUI 右上角「复制链接」直接复制
> 一个带 token、手机能打开的链接。

**token 的"绑定"三种走法，任选其一：**

1. **一次性 URL**：打开 `?token=…` → 令牌存进 localStorage + 服务端下发 `HttpOnly` cookie
   → 之后直接开 `http://<地址>:12688/` 就能用；
2. **界面里填**：顶栏点 🔑，或页面出现"需要访问令牌"面板时，把令牌粘进输入框点「连接」；
3. **命令行/客户端**：`Authorization: Bearer <token>`。

> 为什么要这一步：浏览器里的 `fetch` / `EventSource` 默认不带自定义头，
> 所以 WebUI 无法自己加 `Authorization: Bearer`。用 localStorage + 头 + cookie 三重兜底，
> 前端一行都不用改。`/health` 和静态资源始终免鉴权。
>
> **症状对照**：能看到面板框架但内容全空 = `/api` 被 401 挡了 = 没带令牌。
> 现在这种情况会直接显示"需要访问令牌"面板，不再是空白页。
>
> **症状对照 2**：手机上打开是"无法访问此网站" = 该地址上没有监听。
> 先 `ss -tlnp | grep 12688` 看 deck 到底绑在哪，再看这个地址。

### 2.3 WebUI 上能干什么

| 按钮 | 作用 |
|---|---|
| **编辑** | 改这条 entry 的**启动脚本**、端口、参数、环境变量、就绪路径、TTL；改完直接「保存并启动」试跑，**不用碰 YAML**。脚本不存在会被拒绝；脚本里的 `--port` 会自动同步到端口 |
| **🔒 锁定** | 锁住的模型**不会被 idle 自动卸载**，也**不会被别的模型挤掉**（显存不够时优先牺牲没锁的）。测试 ASR+TTS 这种场景锁上，省掉每次请求的冷启动延迟。状态写进 YAML，重启还在 |
| **日志 / 清屏** | 看某个 entry 的进程日志；「清屏」清掉当前显示 |
| **Events 面板的「清空」** | 清掉操作记录 |
| **失败芯片上的 ✕** | 清掉"启动失败"状态（条目回到 stopped），否则那个红点会一直挂着 |
| **Dashboard 的「复制链接」** | 复制一个带 token、手机能直接打开的地址 |

> 锁定 = YAML 里的 `pinned: true`。调度器（`internal/sched`）里，
> 锁住的条目既不进 idle 回收，也不进驱逐名单。

### 2.4 API 客户端（one-api 等）

**这里有两个不同的 token，别搞混：**

| token | 在哪 | 谁用 | 值 |
|---|---|---|---|
| **deck 令牌**（`server.token`） | 主力机 `~/.config/modeldeck/entries.yaml` | one-api 渠道的「密钥」 | `deck-515c6733b75278fd81a61a3467598ae3` |
| **one-api 令牌**（`sk-xxx`） | one-api 管理面板 →「令牌」页自己创建 | 你的客户端（chatbox / reitti / 脚本） | one-api 内部生成 |

**不用翻文件**，一条命令全给你：

```sh
cd ~/mycode/modeldeck && ./deck token
```

```
token      : deck-515c6733b75278fd81a61a3467598ae3
本机 base   : http://127.0.0.1:12688
局域网 base : http://192.168.10.1:12688   ← 填到 one-api 渠道
webui      : http://127.0.0.1:12688/?token=deck-515c…
one-api 渠道（新建渠道时填这些）:
  类型     : OpenAI
  Base URL : http://192.168.10.1:12688
  密钥     : deck-515c6733b75278fd81a61a3467598ae3
  模型     : gemma-rp, gemma4-qat-26b-a4b-vl-un-chat, … 共 59 个
```

> ⚠️ Base URL **不能填 127.0.0.1**：那是你自己机器的 loopback，
> m64 上的 one-api 会连到它自己。必须填 `192.168.10.1`（网线直连地址）。

**one-api 建渠道步骤**（m64 上 one-api 跑在 `:3000`，原生二进制
`~/app/one-api/build/bin/one-api`）：

1. 渠道 → 添加渠道
2. 类型：`OpenAI`
3. Base URL：`http://192.168.10.1:12688`
4. 密钥：`deck-515c6733b75278fd81a61a3467598ae3`
5. 模型：填要暴露的模型名（= deck 的 entry id，如 `jina-v5-embed`、`jina-rerank`），
   不要用「一键获取全部」以外的随机串
6. 分组/优先级按需

**先验证通道再点测试**（测试会触发 30–60s 冷启动，one-api 可能判超时）：

```sh
# 在主力机上预热
./deck start jina-v5-embed

# 在 m64 上直连 deck 验证（绕过 one-api）
curl -s http://192.168.10.1:12688/v1/embeddings \
  -H "Authorization: Bearer deck-515c6733b75278fd81a61a3467598ae3" \
  -H 'Content-Type: application/json' \
  -d '{"model":"jina-v5-embed","input":"hello"}'
```

实测从 m64 走 deck → jina-v5-embed，1.2s 返回真实向量 ✅

> one-api 的「测试渠道」发的是 **chat** 请求。embed/rerank 类模型没有 chat 端点，
> 测它们要选对应类型的模型，或者在 one-api 里跳过测试直接用。

### 2.5 防火墙

主力机 ufw 已放行：`22 4000 8080 8888 11435 11436 11437 11438 11439 11440 12688 12888 …`

```sh
sudo ufw status verbose          # 查看
sudo ufw allow 12688/tcp         # 需要新增时
```

> 曾经"连不上"的根因不是防火墙，而是 deck 只绑了 `127.0.0.1`。
> **先看 `ss -tlnp | grep 12688` 确认监听地址**，再看防火墙。
> 现在默认绑 `0.0.0.0`，只要 ufw 放行了就能通。

---

## 3. 配置要点（`~/.config/modeldeck/entries.yaml`）

```yaml
server:
    listen: 0.0.0.0:12688                        # 所有网卡；可写多个地址，逗号分隔
    token: ""                                     # 空 = 不鉴权（推荐用 .env 注入）
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

- `listen` 默认 **`0.0.0.0:12688`**：所有网卡都监听，DHCP/Tailscale 换地址不用改配置。
  也可以逗号分隔写多个具体地址，能绑上的都绑，绑不上的只告警不退出。
- 改了 `listen`/`token`/`label` 要重启才生效（systemd: `systemctl --user restart modeldeck`）。
- `label` 是给人看的机器名；手机上 `hostname` 不可用，靠 `label` + getprop 型号区分。
- **令牌不要写进 YAML**：写进项目目录的 `.env`，由 systemd `EnvironmentFile=` /
  `start-termux.sh` 注入环境变量（见 2.1）。

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
（`WorkingDirectory`/`ExecStart` 指向当前目录），并加上
`EnvironmentFile=<项目>/.env` —— 令牌由 `.env` 注入环境变量，YAML 里不用写 token。

### 4.2 Termux（lgv35 / mate10 / gt20）

没有 systemd，继续用脚本：

```sh
cd ~/modeldeck && git pull --ff-only
./update_build.sh          # 需要 Termux 里装了 go（pkg install golang）
./start-termux.sh restart  # 停旧进程 + nohup 后台重启
./start-termux.sh status   # 健康检查 + entry 列表
./start-termux.sh logs     # 跟日志
```

`start-termux.sh` 负责：自动 `termux-wake-lock`、改写配置里的 `listen`、
从 `./.env` 导出 `DECK_TOKEN`、后台启动、写 pidfile、就绪探测，
**启动完成后直接打印令牌**：

```
→ up (pid 12345)
→ token  : deck-6be41b9aee09471b373639886310548b
→ env    : /data/data/com.termux/files/home/modeldeck/.env
```

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

### 控制面 API（带令牌）

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/status` | 全部状态：entry 列表、预算、主机硬件、**可达地址 `host.urls`** |
| GET | `/api/entries/<id>/config` | 读一条 entry 的可编辑配置 + 脚本解析结果 |
| POST | `/api/entries/<id>/config` | 改配置（`script`/`port`/`args`/`env`/`ttl`/`ready_path`…），**写回 YAML** |
| POST | `/api/entries/<id>/lock` | 锁定/解锁（`{"locked":true}`），写回 YAML |
| POST | `/api/entries/<id>/reset` | 清掉"启动失败"状态 |
| POST | `/api/entries/<id>/{start,stop,restart}` | 启停（`?async=1` 立即返回） |

```sh
TOK=$(grep -oP 'DECK_TOKEN=\K\S+' ~/modeldeck/.env)
# 改启动脚本
curl -s -X POST http://127.0.0.1:12688/api/entries/gemma-rp/config \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"script":"/home/roychong/LLM/gemma-rp.sh","port":12888}'
# 锁住，别被 idle 卸载
curl -s -X POST http://127.0.0.1:12688/api/entries/gemma-rp/lock \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"locked":true}'
```

---

## 7. 故障速查

| 现象 | 多半是 | 处理 |
|---|---|---|
| one-api 报连接失败 | deck 没绑对外地址 | `ss -tlnp \| grep 12688`；默认已是 `0.0.0.0` |
| 手机打开 WebUI 是空白/连不上 | 那个地址上没有监听（老配置绑死了 IP） | 改 `listen: 0.0.0.0:12688` 后重启；见 §8.8 |
| 手机打不开 192.168.10.x 的设备 | 两个网段之间没有路由 | 手机用同网段的地址（192.168.100.x）或 Tailscale 地址 |
| WebUI 有框架但内容空白 | `/api` 被 401 挡了，没带令牌 | 顶栏 🔑 填令牌，或开 `?token=…` 一次 |
| WebUI 打开是"API 不可用" | 同上（旧版本前端） | 升级到 v0.2.0+ 后会显示"需要访问令牌"面板 |
| 换了新设备后 401 | 新设备没有 `.env` | `deck token --write` 生成，再把 token 填进 one-api 渠道 |
| 冒烟测试莫名全挂 | shell 里导出了 `DECK_TOKEN` | `unset DECK_TOKEN`（冒烟脚本已自带） |
| WebUI 表格挤成一团 | 旧版本前端 | 确认设备已到 v0.2.0+（`deck version`） |
| one-api 里的手机渠道被自动禁用 | one-api 的 LAN 探测没带密钥 → 401 | 已修（§8.9）；升级 one-api 后会自动恢复 |
| 启动失败的红点一直挂着 | 失败状态不会自己消失 | Events 面板点那个 ✕，或 `POST /api/entries/<id>/reset` |
| deck 起来就崩（手机） | 用了 linux/arm64 交叉编译 | 改用 `GOOS=android` 重新编译 |
| 模型启动失败 "port in use" | 有手工起的同名后端占着端口 | `ss -tlnp \| grep <port>` 找到并停掉 |
| 显存不够/OOM | 见 §8.1 | 调度器的账还没算对（待修） |
| 服务重启后模型全没了 | 正常行为 | 重新请求即冷启动；`serve -keep` 可保留模型 |

---

## 8. 踩过的坑（别再踩）

### 8.8 绑死 IP = 给自己埋雷
最早主力机绑 `127.0.0.1:12688,192.168.10.1:12688`，于是：

- 手机只连 WiFi（`192.168.100.200`），那个地址上**没有监听** → 浏览器直接连不上；
- WiFi 地址是 DHCP 的，租约一换，绑死的地址就不存在了；
- Tailscale 的 `100.x` 地址根本没法预先写进配置。

→ **统一 `0.0.0.0:12688`**。deck 启动时枚举本机所有可用地址（跳过 docker/virbr 等
虚拟网卡），把 URL 打进横幅、WebUI 的「地址」卡片也能一键复制。
需要固定端口给别人访问时，用防火墙限制来源，而不是绑地址。

### 8.9 one-api 的 LAN 探测要带密钥
one-api fork 里有个自定义的 LAN supervisor（`controller/channel_local_probe.go`），
每 60 秒对 IP 字面量渠道做三步探测：ICMP → TCP → `GET {base}/v1/models`。
**第三步必须带渠道自己的 `Authorization: Bearer <key>`**，
否则 deck 返回 401 → 判定 `service_down` → 把渠道自动禁用（理由：
`local probe: readiness endpoint returned 401`）。
→ 修在 one-api 侧（提交 `18f2ccdb`），模型侧不背这个锅。

### 8.10 `.env` 不能被服务端直接读
一开始让 deck 自己去找二进制旁边的 `.env`，结果同机第二个 deck 实例（测试用）
也被迫鉴权 → 冒烟 40 项全挂。
→ 服务端只认「YAML → `$DECK_TOKEN`」，`.env` → 环境变量由部署层做。

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
