# Hive

> 🐝 **Docker for Agents** —— 一套让多 Agent 可以分工复用的能力级虚拟化系统。

**项目状态**：MVP 跑通 —— 核心架构（runtime + namespace 隔离 + Rank + quota + LLM 代理 + Go SDK）全部就绪，`make demo` 一键演示 "连接共享、配额隔离" 的核心不变量。**尚未生产就绪**，TODO 见文末。

相关文档：
- [`ARCHITECTURE.md`](ARCHITECTURE.md) —— 产品愿景与术语（招聘/职场类比、Rank 六类权限、共享连接 vs 隔离配额）
- [`DEMO_PLAN.md`](DEMO_PLAN.md) —— 当前版本的实现方案与里程碑拆解
- [`CLAUDE.md`](CLAUDE.md) —— 给后续协作者（含 AI 编码助手）的速查

---

## 5 分钟跑起来

**前置**：Linux、Go 1.22+、root（需要 `CLONE_NEWNS`/`CLONE_NEWNET`）、Python 3（仅 demo 里起本地 HTTP 服务用）。

```bash
# 编译所有二进制（hived / hive / 四个示例 Agent）
make build

# 一键端到端演示
sudo ./scripts/demo.sh
```

脚本会：

1. 起 `hived` 守护进程
2. 打包 `examples/{fetch,upper,summarize}` 成 Hive Image
3. 用 Hivefile 拉起两个 Room（`demo-room-a` / `demo-room-b`）
4. Room A 里连续打 5 次 fetch，第 6 次被 `intern` Rank 的 API 配额挡住
5. Room B 单独打 1 次 fetch —— 配额不受 Room A 影响
6. 两个 Room 各跑一次 summarize，独立扣减 token 配额
7. `hive team` 展示两边剩余配额的差异

没有 `OPENAI_API_KEY` 时自动走内置 mock provider；要用真 LLM：

```bash
OPENAI_API_KEY=sk-... sudo -E ./scripts/demo.sh
```

---

## 手动体验

```bash
# 起 daemon
./bin/hived &

# 打包一个 Agent
./bin/hive build ./examples/echo
./bin/hive images

# 创建 Room 并招聘 Agent
ROOM=$(./bin/hive init my-room)
./bin/hive hire "$ROOM" echo:0.1.0
./bin/hive team "$ROOM"

# 跑任务（stdin 接收任务 JSON，stdout 接收流式日志）
./bin/hive run "$ROOM" '{"hello":"world"}'

# 收工
./bin/hive stop "$ROOM"
```

## 命令速查

| 命令 | 说明 |
|---|---|
| `hive ping` | 检查 daemon 是否在 |
| `hive version` | 打印 CLI + daemon 版本 |
| `hive build <dir>` | 把一个 Agent 源码目录打包为 Hive Image |
| `hive images` | 列出本地 Image |
| `hive init <name>` | 创建新 Room，返回 RoomID |
| `hive rooms` | 列出所有 Room |
| `hive hire <room> <image>` | 把 Agent 招进 Room（`--rank <name>` 覆盖默认） |
| `hive up <hivefile>` | 按 Hivefile 声明一次性建 Room + 招聘所有 Agent |
| `hive team <room>` | 列出 Room 内 Agent 及配额剩余 |
| `hive run <room> [task]` | 下发任务，实时流式打印 Agent 日志（`--target <image>` 选收件人） |
| `hive stop <room>` | 停掉 Room |

---

## 写一个自己的 Agent

Agent 是任何能在 stdin/stdout 上讲 JSON-RPC 2.0 的进程。Go 语言有现成 SDK：

```go
package main

import (
    "context"
    hive "github.com/anne-x/hive/sdk/go"
)

func main() {
    a := hive.MustConnect()
    defer a.Close()

    ctx := context.Background()
    for task := range a.Tasks() {
        a.Log("info", "got task")
        _, body, err := a.NetFetch(ctx, "GET", "https://example.com", nil, nil)
        if err != nil {
            task.Fail(1, err.Error())
            continue
        }
        task.Reply(map[string]any{"bytes": len(body)})
    }
}
```

配一份 `hive.yaml`：

```yaml
name: my-agent
version: 0.1.0
entry: bin/my-agent
rank: staff              # 默认 Rank
capabilities:
  requires: [net]
  provides: [my-skill]
```

构建之后 `hive build ./my-agent`，就能 hire 进任意 Room。

别的语言（Python / Rust / TS / …）直接按 JSON-RPC 2.0 读写 stdin/stdout 即可 —— wire 协议语言无关。

### Agent ↔ Hive 方法集

| 方向 | Method | 说明 |
|---|---|---|
| Hive → Agent | `task/run` | 下发任务 |
| Hive → Agent | `peer/recv` | 同 Room peer 发来的消息 |
| Hive → Agent | `shutdown` | 温和终止 |
| Agent → Hive | `fs/read` `fs/write` `fs/list` | 受 Rank 约束的文件 I/O |
| Agent → Hive | `net/fetch` | HTTP 请求（扣 `api_calls` 配额） |
| Agent → Hive | `llm/complete` | LLM 调用（扣 token 配额） |
| Agent → Hive | `peer/send` | 给同 Room 的指定 Agent 发消息 |
| Agent → Hive | `task/done` `task/error` | 任务终态 |
| Agent → Hive | `log` | 结构化日志 |

---

## Rank（权限 + 配额职级）

内置四档：

| Rank | 文件系统 | 网络 | LLM | 默认配额 |
|---|---|---|---|---|
| `intern` | 读 `/app` `/tmp`；写 `/tmp` | ✓ | ✗ | http=5 |
| `staff` | 读 `/app` `/tmp` `/data`；写 `/tmp` `/data` | ✓ | ✓ | http=20, tokens(gpt-4o-mini)=5000 |
| `manager` | 读 `/`；写 `/tmp` `/data` | ✓ | ✓ | http=200, tokens=50000 |
| `director` | 全权限 | ✓ | ✓ | 无限 |

Hivefile / `hive hire --rank` 可覆盖默认 Rank。权限和配额由 `hived` 在代理层统一 enforce —— Agent 进程内核级看不到别的 Room，语义级 I/O 也跑不过 Hive 的代理层。

---

## 架构速览

```
┌──────────────── hived 进程 (Go, root) ──────────────────┐
│                                                          │
│   CLI 客户端 ──▶ Unix socket ──▶ IPC dispatcher          │
│                                      │                    │
│                                      ▼                    │
│     ┌───── Room  (mount+net namespace 独立) ─────────┐    │
│     │   Router (channel 路由同 Room peer 消息)       │    │
│     │   Agent Conns ──stdio──▶  Agent 子进程         │    │
│     │                 (pivot_root 进 rootfs)         │    │
│     └────────────────────────────────────────────────┘    │
│                                                          │
│   QuotaActor (单 goroutine + channel，按 (Room,Agent) 计数)│
│   netproxy / llmproxy  (共享 http.Client 连接池)          │
└──────────────────────────────────────────────────────────┘
```

### 核心不变量

来自 `ARCHITECTURE.md` §"共享连接 vs 隔离配额"：

- **连接共享**：跨所有 Room 和 Agent，同一个 API key 只维护一条 `http.Transport`，外部请求通通复用连接池
- **配额隔离**：按 `(RoomID, AgentName, 资源)` 三元组独立计数；任何 Agent 触顶不影响其他 Agent 或 Room

---

## 目录结构

```
cmd/                      CLI + daemon 入口
├── hive/                 hive CLI
└── hived/                hived daemon

internal/
├── protocol/             JSON-RPC 2.0 + NDJSON wire
├── rpc/                  Agent ↔ Hive 方法集 + params
├── ipc/                  CLI ↔ daemon 方法集 + client/server
├── image/                Hive Image manifest + Ref
├── store/                本地 image store (~/.hive/images)
├── hivefile/             Hivefile.yaml 解析
├── agent/                Agent 子进程封装（stdio JSON-RPC 双向泵）
├── router/               Room 内 peer 消息路由（channel actor）
├── room/                 Room 生命周期 + Agent 督导
├── ns/                   Linux 沙箱（CLONE_NEWNS/NEWNET + pivot_root）
├── rank/                 Rank 模板 + 六类权限结构
├── quota/                Quota actor（channel-based）
├── proxy/
│   ├── fsproxy/          fs/{read,write,list}
│   ├── netproxy/         net/fetch + 共享 http.Client
│   └── llmproxy/         llm/complete + Provider 抽象
└── daemon/               IPC handlers 接线

sdk/go/                   Go Agent SDK（channel 化 API）

examples/
├── echo/                 最小 Agent，raw JSON-RPC
├── fetch/                intern rank，演示 net/fetch + API 配额
├── upper/                staff rank，纯本地 + peer 消息
└── summarize/            staff rank，演示 llm/complete + token 配额

hivefiles/demo/           demo 用的两份 Hivefile
scripts/demo.sh           一键端到端演示
```

---

## 测试

```bash
make test           # 所有单元测试
go test -race ./... # 带 race detector
make demo           # 端到端 smoke（需要 root）
```

覆盖到的包：`protocol`、`quota`（含并发测试）、`router`、`image`、`store`、`hivefile`、`rank`、`fsproxy`（含跨 Room 隔离）、`llmproxy`（含跨 Room token 隔离）。

未覆盖（依赖进程/内核状态，靠 `demo.sh` 做集成验证）：`ns`、`agent`、`daemon`、`room`、`ipc`。

---

## TODO

### 🔧 近期（MVP 里遗留的小坑）

- [ ] **Hivefile `quota:` override 未生效**：`hivefile.AgentEntry.Quota` 和 `ipc.AgentHireParams.QuotaOverr` 解析了但 daemon 没 apply。修在 `internal/daemon/daemon.go:installAgentProxies`。
- [ ] **`capabilities.requires`/`provides` 未校验**：manifest 声明 `requires: [llm]` 但被 hire 成 `intern`（无 LLM）目前不会在 hire 时报错，要等运行期第一次 `llm/complete` 才失败。
- [ ] **`hive up` 不支持 `--room <name>` 覆盖**：演示多 Room 要备多份 Hivefile。加一个 `--room` 标志。
- [ ] **`hive hire` 不支持 `--quota k=v` 覆盖**：只能覆盖 rank，不能只改某一个资源的配额。
- [ ] **`hive logs <room>` 没做**：`ipc` 里预留了位置但 handler 和 CLI 都没实现。Agent stderr 落到 `~/.hive/rooms/<id>/logs/` 里但没有聚合视图。
- [ ] **demo.sh 的 `set -o pipefail` 坑**：第 6 次 fetch 的 check 走 `out=$(...||true)` 兜底；更干净的做法是拆个 helper 函数。
- [ ] **Agent 崩溃的诊断信息难回传**：`ns.RunInit` 里的 error 走 cmd.Stderr → 落盘 log 文件；CLI 端看到的是"agent exited"笼统报错，不便于调试。考虑把 init 阶段 error 通过一个额外的 pipe 回传给 daemon。
- [ ] **Agent 日志没 rotation**：`~/.hive/rooms/<id>/logs/*.stderr.log` 无限增长。

### 🧱 中期（架构内稳健性）

- [ ] **集成测试（Go）**：目前只有单元测试 + `demo.sh`。补一个 `TestEndToEnd`，exec hived 起来、走 IPC 全链路、断言 `hire → run → team` 结果。
- [ ] **Rank 级 peer 策略**：`room.Hooks.AuthPeerSend` 已经预留，但 demo 里所有同 Room peer 都放行。加个 Rank 粒度的 allow-list（intern 只能给 manager 发消息之类）。
- [ ] **Capabilities 匹配**：`requires` 和 `provides` 在 hire 时真 enforce（例如 Hivefile 里所有 `requires` 必须有对应的 `provides`）。
- [ ] **更多 LLM provider**：现在只有 `mock` 和 `openai`（OpenAI-compatible）。加 `anthropic`、配置驱动的 provider routing。
- [ ] **`hive exec <room> <agent> <cmd>`**：类似 `docker exec`，给运行中的 Agent 注入一次性任务。
- [ ] **daemon 重启 Room 持久化**：目前 daemon 死了 Room 全灭；Room 状态应能从 `~/.hive/rooms/` 恢复。
- [ ] **`lo` 接口补齐**：`CLONE_NEWNET` 默认 loopback 是 down 的，有些 Agent 内部库会意外失败。init 阶段 `ip link set lo up` 一下（或 Go 语言版的 netlink）。
- [ ] **Agent 输出压缩/分片**：`fs/read` 大文件目前整包 base64 JSON 回传，无流式。加 `fs/read-stream` 或 chunked 语义。

### 🚀 v2（`DEMO_PLAN.md` 里明确列为"不做"的大特性）

- [ ] **seccomp-bpf syscall 白名单**：生产级沙箱补强，防止内核漏洞提权。
- [ ] **user namespace + uid remap**：脱离 root 运行 daemon。
- [ ] **OCI-style 层状镜像**：取代当前的"复制整个目录"策略，支持层缓存、内容寻址、digest 校验。
- [ ] **远端 Registry（`hive push` / `hive pull`）**：把本地 store 变成对等的 Hive Hub。
- [ ] **跨 Room 通信**：有受控方式让 Room A 的 Agent 跟 Room B 的 Agent 对话（等价于 docker networks）。
- [ ] **跨主机 / 多 daemon 集群**：一个 CLI 连多台机器的 hived（类似 docker swarm）。
- [ ] **非 Linux 支持**：macOS（用 macOS Virtualization.framework？）/ Windows（WSL2？）。
- [ ] **Hivefile 嵌套**：一个 Hive 可以 hire 另一个 Hive（函数调用式）。

### 📚 文档

- [ ] Agent 作者指南（从零写一个 Agent 的 step-by-step）
- [ ] JSON-RPC 方法完整参考（目前零散在 `internal/rpc/`）
- [ ] 非 Go 语言 SDK 写法示例（Python 最小实现）
- [ ] ARCHITECTURE.md 里 §113（Rank 六类权限）与代码里 `rank.Rank` 的映射对照

---

## 许可证

[MIT](LICENSE)。
