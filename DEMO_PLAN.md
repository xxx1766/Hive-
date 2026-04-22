# Hive 最小 Demo 实现方案

## 背景与目标

`ARCHITECTURE.md` 勾勒了 Hive 的产品愿景 —— "Docker for Agents"。本 plan 把愿景落到一个**能跑的最小 demo**：验证核心架构可行，给后续迭代打好地基。

**demo 成功的判定标准（一条）：**

> 一台 Linux 机器上，同时运行两个 Room（`room-A`、`room-B`），各自 hire 同一批 Agent。room-A 的 `summarize` Agent 吃掉自己那份 LLM token 配额，**不影响** room-B 的配额；room-A 的文件系统里 **看不到** room-B 的任何路径；两个 Room 背后共享**同一个** OpenAI TCP 连接池。

这正是 `ARCHITECTURE.md:140-167` "共享连接 vs 隔离配额" 的端到端兑现。

## 已锁定的核心决策（来自 4 轮问答）

| 决策点 | 选择 | 影响 |
|---|---|---|
| 实现语言 | Go | daemon 用 channel 做 router / quota actor / supervisor |
| 平台 | 仅 Linux | namespace 原语可用；不处理 Windows/macOS |
| Demo 范围 | 跨 Room 隔离 | 至少两个 Room，独立配额，mount/network ns 实际生效 |
| Agent 协议 | Unix socket + JSON-RPC | wire 语言中立；Agent 可用任意语言写 |
| Rank 强制 | `CLONE_NEWNS` + `CLONE_NEWNET` + Hive 代理所有对外 I/O | 内核级隔离 Room，语义级权限走 daemon |
| 进程模型 | 常驻 daemon `hived` + `hive` CLI | CLI 通过 Unix socket 连 daemon；多 Room 共存 |
| Demo Agent | `fetch` + `upper` + `summarize`（真 LLM） | 前两个做可重复自测，第三个演示 LLM 配额扣减 |

## 组织结构

```
/data/Hive-
├── cmd/
│   ├── hive/              # CLI client 入口（瘦，主要转发 RPC 到 hived）
│   └── hived/             # daemon 入口
├── internal/
│   ├── daemon/            # hived 主 server：监听 Unix socket，派发命令
│   ├── room/              # Room 生命周期、rootfs 管理、Agent 督导
│   ├── agent/             # 单个 Agent 进程封装（spawn, stdio loop, teardown）
│   ├── router/            # 同 Room peer-to-peer 消息路由（channel 驱动）
│   ├── rank/              # Rank 定义 + 权限/配额判定器
│   ├── quota/             # quota actor（单 goroutine + channel，负责计数）
│   ├── proxy/
│   │   ├── fsproxy/       # 文件 I/O 代理（read/write/list，受 Rank 约束）
│   │   ├── netproxy/      # HTTP 代理：共享连接池 + per-(Room,Agent) 配额
│   │   └── llmproxy/      # LLM provider 抽象（openai / anthropic / mock）
│   ├── ns/                # Linux namespace 设置（Cloneflags, bind-mount, pivot_root）
│   ├── image/             # Hive Image 打包与解析（目录 + agent.yaml manifest）
│   ├── store/             # 本地 image store（~/.hive/images/）
│   ├── ipc/               # CLI ↔ daemon 协议（JSON over Unix socket）
│   └── protocol/          # Hive ↔ Agent JSON-RPC schema + codec
├── sdk/
│   └── go/                # Go Agent SDK：把 wire 协议包成 channel-based API
├── examples/
│   ├── fetch/             # intern 级 Agent：HTTP 拉取
│   ├── upper/             # staff 级 Agent：文本大写化
│   └── summarize/         # staff 级 Agent：调 LLM 做摘要
├── hivefiles/
│   └── demo/
│       └── Hivefile.yaml  # demo 用的 Room 声明
├── scripts/
│   └── demo.sh            # 一键跑完整 demo 的脚本
├── ARCHITECTURE.md
├── CLAUDE.md
├── DEMO_PLAN.md           # 本文件
├── README.md
├── go.mod
└── Makefile
```

**内部通信拓扑（channel 视角）：**

```
                        hived 进程
  ┌─────────────────────────────────────────────────────────────┐
  │                                                              │
  │   CLI socket ──▶ dispatcher ──▶ room/Room (1 per Room)       │
  │                                       │                      │
  │                                       ▼                      │
  │              ┌──────────────── router.routes ────┐           │
  │              │                                    ▼           │
  │     agent.Conn ─inbox◀─┐      ┌─agent.Conn ─inbox◀─┐         │
  │     agent.Conn ─outbox─┘      └─agent.Conn ─outbox─┘         │
  │         │                             │                      │
  │         ▼                             ▼                      │
  │      stdin                          stdin                    │
  │      stdout (of Agent process 1)    stdout (of Agent 2)      │
  │                                                              │
  │   quota.Actor (全局一个 goroutine；channel 串行扣减)          │
  │   netproxy.Pool  (全局共享；per-key 维护 http.Client)         │
  └──────────────────────────────────────────────────────────────┘
```

## 关键数据格式

### `agent.yaml`（Image manifest，放在 Agent 源码目录根）

```yaml
name: summarize
version: 0.1.0
entry: ./bin/summarize           # 构建后 binary 相对路径
rank: staff                      # 默认 Rank，可被 Hivefile 覆盖
capabilities:
  requires: [net, llm]           # Agent 声明需要的能力类别
  provides: [summarize]          # Agent 声明自己对外提供什么
quota:                           # 默认配额，Hivefile 可覆盖
  tokens:
    "openai:gpt-4o-mini": 10000
  api_calls:
    "http": 50
```

### `Hivefile.yaml`（Room 声明）

```yaml
room: demo-room-a
agents:
  - image: fetch:0.1.0
    rank: intern
  - image: upper:0.1.0
    rank: staff
  - image: summarize:0.1.0
    rank: staff
    quota:
      tokens:
        "openai:gpt-4o-mini": 5000    # 覆盖该 Agent 在此 Room 的配额
```

### Hive ↔ Agent JSON-RPC 方法集（demo 范围）

| 方向 | Method | 说明 |
|---|---|---|
| Hive → Agent | `task/run` | 下发任务，包含 goal 和任意 params |
| Hive → Agent | `peer/recv` | 通知 Agent：同 Room 的某 peer 发来消息 |
| Hive → Agent | `shutdown` | 温和终止 |
| Agent → Hive | `fs/read` / `fs/write` / `fs/list` | 文件 I/O（受 Rank 约束，走 fsproxy） |
| Agent → Hive | `net/fetch` | HTTP 请求（走 netproxy，扣 api_calls 配额） |
| Agent → Hive | `llm/complete` | LLM 调用（走 llmproxy，扣 tokens 配额） |
| Agent → Hive | `peer/send` | 给同 Room 指定 Agent 发消息（走 router，Rank 检查） |
| Agent → Hive | `task/done` / `task/error` | 任务终态上报 |
| Agent → Hive | `log` | 结构化日志，统一被 daemon 收 |

### CLI ↔ hived 协议

Unix socket `$XDG_RUNTIME_DIR/hive/hived.sock`（fallback `~/.hive/hived.sock`）。使用 JSON-framing（换行分隔）。命令集：

```
hived init-room    name=<str>   → {roomId}
hived build        path=<dir>   → {image}
hived hire         room=<id> image=<name:ver> [rank=<r>] [quota=<json>]
hived run          room=<id> task=<json>   → 流式输出 log/status
hived team         room=<id>               → 当前 Room 的 Agent 列表
hived ps                                   → 所有 Room
hived logs         room=<id>               → 历史日志
hived stop-room    room=<id>
```

### 目录/路径约定

| 用途 | 路径 |
|---|---|
| daemon socket | `$XDG_RUNTIME_DIR/hive/hived.sock` |
| local image store | `~/.hive/images/<name>/<version>/` |
| Room 根 | `~/.hive/rooms/<roomId>/` |
| Room rootfs（mount 基座） | `~/.hive/rooms/<roomId>/rootfs/` |
| Room 日志 | `~/.hive/rooms/<roomId>/logs/` |
| Room 状态 | `~/.hive/rooms/<roomId>/state.json` |

## 实现里程碑

每个里程碑都是一个可独立验收的交付。

### M1：骨架与协议（1 天）

- `go.mod` 初始化（module: `github.com/anne-x/hive`，Go 1.22+）
- 目录骨架（上面的树）
- `internal/protocol`：定义 JSON-RPC 消息类型、codec
- `internal/ipc`：定义 CLI ↔ daemon 协议
- `cmd/hive` 和 `cmd/hived` 各自编出一个能跑的二进制：`hive --version` / `hived --version`
- `Makefile`：`make build` / `make test`
- **验收：** `make build` 成功，两个命令输出版本号

### M2：daemon 与 echo Agent 跑通（1.5 天，暂不启用 namespace）

- `hived` 监听 Unix socket，接受 `init-room` / `run`
- `hive init my-room` → daemon 创建 Room 目录，返回 roomId
- 硬编码一个 `echo` Agent（直接 exec，无 namespace）：收到 `task/run` 原样回 `task/done`
- `hive run my-room` → daemon fork agent，建立 stdio JSON-RPC 管道，转发日志到 CLI
- `internal/router` 的最小版本：一个 Room 一条 `chan Msg`
- **验收：** `hive init + hive run` 能看到 echo Agent 的 "hello" 回显

### M3：namespace 隔离 + Hivefile + 多 Agent（2 天）

- `internal/image` + `internal/store`：`hive build ./examples/fetch` → 把目录拷到 `~/.hive/images/fetch/0.1.0/`
- `internal/ns`：spawn Agent 时带 `CLONE_NEWNS | CLONE_NEWNET`；bind-mount Room 的 rootfs（`/usr` 只读 + Room 私有 `/data` 和 `/tmp`）；`pivot_root` 切根
- `hive hire my-room fetch:0.1.0 --rank intern`：把 image copy 进 Room 的 rootfs
- Hivefile 解析：`hive run -f Hivefile.yaml` 按声明一次性 hire 多个 Agent
- `internal/router` 支持 peer/send / peer/recv，带 Rank 级 "谁能给谁发消息" 检查（demo 里先允许同 Room 内全联通）
- **验收：**
  - room-A 和 room-B 都 hire `fetch`
  - room-A 的 `fetch` `fs/list /` 看不到 room-B 的任何路径
  - room-A 的 `fetch` 尝试 `connect()` 直接出网 → 失败（network ns 无路由）

### M4：Rank 代理 + 真 I/O + 配额（2 天）

- `internal/rank`：定义 intern/staff/manager 模板 + 六大类权限结构
- `internal/proxy/fsproxy`：实现 `fs/read` / `fs/write` / `fs/list`，按 Rank 允许/拒绝路径
- `internal/proxy/netproxy`：实现 `net/fetch`；**per-key 共享连接池**（`http.Transport` 全局复用）；per-(Room, Agent) `api_calls` 配额
- `internal/quota`：quota actor（单 goroutine + `chan QuotaReq`），串行原子扣减
- `internal/proxy/llmproxy`：定义 `Provider` 接口，实现 `openai`（OpenAI-compatible，读 `OPENAI_API_KEY` / `OPENAI_BASE_URL`）和 `mock`（返回 canned response）
- 示例 Agent：`fetch`（用 raw JSON-RPC 调 `net/fetch`）+ `upper`（纯本地字符串处理，无对外 I/O）
- **验收：**
  - `fetch` 超配额后再 `net/fetch` 收到 quota_exceeded error
  - 两个 Room 的 `fetch` 互不影响对方配额
  - `netstat` / `ss` 能看到同一 OpenAI host 只有一条 established 连接（配额隔离但连接共享）

### M5：Go Agent SDK + summarize + 端到端演示（1.5 天）

- `sdk/go`：
  - `hive.Connect()` 返回 `*Agent`，内部起两个 goroutine 做 stdio 双向泵
  - `agent.Tasks() <-chan Task` —— 接任务的 channel
  - `agent.Reply(id, result)` / `agent.LLMComplete(ctx, params)` / `agent.PeerSend(to, msg)` 等包装方法
  - channel + context 一体化，提供 Go 语义的顺手 API
- `examples/summarize/main.go`：用 SDK 写，接受任务 → 调 `llm/complete` → 返回摘要
- `scripts/demo.sh`：端到端脚本
  1. 起 `hived`
  2. `hive build` 三个 Agent
  3. `hive run -f hivefiles/demo/Hivefile.yaml`（room-A）+ 同样起 room-B
  4. 对 room-A 发送一条 `summarize` 任务，消耗 4000 tokens
  5. `hive team room-A` / `hive team room-B` 显示两边配额对比
- 中文 `README.md`：五分钟上手指南
- **验收：** `./scripts/demo.sh` 一键跑完，输出清晰展示"连接共享 + 配额隔离"证据

## 总计工作量估算

约 **8 个工作日**（含调试、文档、不含新需求）。按 1.5× 缓冲，**两周**拿到一个能演示的 demo。

## 风险与明确不做的事

**风险：**
1. `CLONE_NEWNS` 需要 `CAP_SYS_ADMIN` 或 root —— demo 跑法可能需要 `sudo hived` 或 user namespace。里程碑 M3 早期先验证本机能否跑通。
2. `pivot_root` 出错会留下 stale mount，需要在 Agent 退出时兜底清理。
3. LLM provider 的 token 计数需要在 response 里解析；先信任 provider 返回的 usage 字段，不自己 tokenize。

**明确不做（留 v2）：**
- Windows / macOS 支持
- 远端 Registry（`hive push` / `hive pull`）
- OCI 层状镜像格式（demo 就是目录复制）
- seccomp-bpf syscall 白名单
- user namespace + uid remap（若不需要 root 运行则引入）
- Agent-to-Agent 跨 Room 通信
- 多 daemon 集群 / 跨主机 Room

## 验证清单（demo 做完后逐条打勾）

- [ ] `make build` 生成 `hive` + `hived` 两个二进制
- [ ] `hived` 启动后在 socket 路径可连
- [ ] `hive build ./examples/<name>` 三个示例 Agent 均能入 store
- [ ] `scripts/demo.sh` 无报错跑完
- [ ] room-A 的 Agent `ls /var/lib/hive/rooms/B` 返回 ENOENT
- [ ] room-A 的 Agent 直连 `api.openai.com:443` 失败（network ns 拒绝）
- [ ] room-A 和 room-B 的 `summarize` 各自跑一次，配额独立扣减
- [ ] `ss -tnp | grep openai` 只看到一条连接（或少于 Room 数）
- [ ] `hive team` 输出里能看到每个 Agent 的当前配额剩余
- [ ] README 按步骤能让第三方用户跑起来

---

**下一步：** 这份 plan 确认后，按 M1 → M5 顺序推进。我会为每个里程碑建一批 TaskCreate 子任务，落到实现时再细化。

---

**MVP 完成之后的架构演进** —— 本 demo plan 故意没覆盖、已单独归档到 `ARCHITECTURE.md` §"架构扩展方向"：

- Hive 与外部 AI 工具（Claude Code / Cursor / MCP / LLM）的依赖方向 —— 决策：**Hive 在上，AI 工具当后端**；新增 `mcpproxy` / `aitoolproxy` 两类代理
- Agent 打包多态（`manifest.kind` 字段）—— 新增 `skill` / `json` 两种形态，配套 `hive-skill-runner` / `hive-workflow-runner` 子系统

对应的 backlog 条目见 `README.md` §"🧱 中期" 与 §"🚀 v2"。
