# Hive

> 🐝 **Docker for Agents** —— 一套让多 Agent 可以分工复用的能力级虚拟化系统。

**项目状态**：MVP 跑通 —— 核心架构（runtime + namespace 隔离 + Rank + quota + LLM 代理 + Go SDK）全部就绪，`make demo` 一键演示 "连接共享、配额隔离" 的核心不变量。**尚未生产就绪**，TODO 见文末。

相关文档：
- [`ARCHITECTURE.md`](ARCHITECTURE.md) —— 产品愿景与术语（招聘/职场类比、Rank 六类权限、共享连接 vs 隔离配额）
- [`DEMO_PLAN.md`](DEMO_PLAN.md) —— 当前版本的实现方案与里程碑拆解
- [`CLAUDE.md`](CLAUDE.md) —— 给后续协作者（含 AI 编码助手）的速查

---

## 5 分钟跑起来

前置：Linux、Go 1.22+、sudo（hived 需要开 namespace）。

```bash
git clone git@github.com:xxx1766/Hive-.git
cd Hive-
sudo ./scripts/install.sh          # 或 PREFIX=$HOME/.local 装到用户目录
```

脚本会检查 Go 版本、`make build`、把 4 个 binary 装进 `/usr/local/bin/`、初始化 `~/.hive/`。

起 daemon + 跑第一个 skill Agent（无需 API key）：

```bash
sudo hived &
ROOM=$(hive init demo)
hive pull github://xxx1766/Hive-/registry/agents/brief
hive hire "$ROOM" brief:0.1.0
hive run  "$ROOM" '{"text":"Hive 是一套面向多 Agent AI 的能力级虚拟化系统。"}'
```

- **完整上手教程**（装好到写第一个自己的 Agent）→ [`docs/TUTORIAL.md`](docs/TUTORIAL.md)
- **一键端到端演示**（跨 Room 隔离、配额、kind=skill、kind=workflow、远端 pull 全走一遍）：

```bash
sudo ./scripts/demo.sh             # 11 场景
OPENAI_API_KEY=sk-... sudo -E ./scripts/demo.sh   # 用真 LLM
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
| `hive hire <room> <ref>` | 把 Agent 招进 Room；`<ref>` 可以是 `name:version`（本地）或远端 URL（三种形式见 §Registry） |
| `hive pull <url>` | 显式把一个远端 Agent 拉到本地 store |
| `hive up <hivefile\|url>` | 按 Hivefile 声明一次性建 Room + 招聘所有 Agent；hivefile 本身和里面的 Agent 都可远端 |
| `hive team <room>` | 列出 Room 内 Agent 及配额剩余 |
| `hive volume create/ls/rm` | 管理跨 Room 持久化卷 |
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

配一份 `agent.yaml`：

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

### 不写代码：`kind: skill` Agent

Agent 也可以**只写一份 markdown**，Hive 用内置 `hive-skill-runner` 驱动 LLM 把它跑起来。适合快速 prototype 或轻量任务。参考 `examples/brief/`：

```
examples/brief/
├── agent.yaml       # manifest (kind: skill)
└── SKILL.md        # Agent 的"灵魂"
```

```yaml
# agent.yaml
name: brief
version: 0.1.0
kind: skill
skill: SKILL.md
model: gpt-4o-mini
tools: [net, fs]       # 允许 skill 调用哪些 Hive proxy
rank: staff
```

```markdown
# SKILL.md
你是一个摘要助手。收到 {"text": ...} 就用一句话总结。
严格按 Hive runtime 约定返回 JSON：{"answer": "..."}。
```

hived 在 hire 时把 hive-skill-runner **hardlink 到 Image 目录**，作为 entry 送进沙箱 —— 对 daemon 而言它仍只是一个普通 Agent 子进程，同样的 namespace / Rank / 配额。runner 内部读 SKILL.md 当 system prompt，驱动一个最多 20 轮的 LLM 循环（ReAct-lite JSON 协议：`{"tool": "...", "args": {...}}` 或 `{"answer": "..."}`）。

### 不写代码也不写 prompt：`kind: workflow` Agent

适合"步骤结构清晰、不想让 LLM 每步自由裁量"的任务。两种模式共用 `cmd/hive-workflow-runner`：

**A. 静态声明式**（`examples/url-summary/`）

```yaml
# agent.yaml
name: url-summary
version: 0.1.0
kind: workflow
workflow: flow.json
tools: [net, llm]
```

```json
// flow.json — 用户完全决定执行顺序
{
  "steps": [
    {"id":"fetch",   "tool":"net_fetch",    "args":{"url":"$input.url"}},
    {"id":"summary", "tool":"llm_complete", "args":{
       "model":"gpt-4o-mini",
       "messages":[
         {"role":"system","content":"Summarise in one sentence."},
         {"role":"user",  "content":"$steps.fetch.body"}
       ]
     }}
  ],
  "output": "$steps.summary.text"
}
```

变量替换规则：`$input.<path>` 来自 task 输入；`$steps.<id>.<path>` 来自前面步骤的 result；整串替换保留原类型（不是字符串插值）。

**B. LLM 规划式**（`examples/research/`）

```yaml
# agent.yaml
kind: workflow
planner: PLANNER.md
model: gpt-4o-mini
tools: [llm]
```

```markdown
# PLANNER.md
给你一个问题，规划一个 ≤3 步的 workflow：先 brainstorm 几个角度，再基于这些角度写最终答案。
只返回一个 JSON workflow 对象，不要 markdown fence。
```

流程：runner 把 task 输入交给 planner LLM，LLM 产生 `flow.json`，runner 校验后 deterministic 地执行。跟 skill 模式（ReAct、每步问 LLM）的区别：**规划只一次**，执行过程不再调 LLM 做决策 —— 省 token、结果更可预测。

两种模式通过 manifest 字段区分：`workflow:` 对应 A，`planner:` 对应 B，不能同时设。

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

| Rank | 文件系统 | 网络 | LLM | Memory | 默认配额 |
|---|---|---|---|---|---|
| `intern` | 读 `/app` `/tmp`；写 `/tmp` | ✓ | ✗ | ✗ | http=5 |
| `staff` | 读 `/app` `/tmp` `/data`；写 `/tmp` `/data` | ✓ | ✓ | ✓ | http=20, tokens(gpt-4o-mini)=5000 |
| `manager` | 读 `/`；写 `/tmp` `/data` | ✓ | ✓ | ✓ | http=200, tokens=50000 |
| `director` | 全权限 | ✓ | ✓ | ✓ | 无限 |

Hivefile / `hive hire --rank` 可覆盖默认 Rank。权限和配额由 `hived` 在代理层统一 enforce —— Agent 进程内核级看不到别的 Room，语义级 I/O 也跑不过 Hive 的代理层。

---

## Registry（GitHub-hosted）

MVP 阶段没搭独立 Registry 服务 —— 直接把 GitHub 公开目录当 registry 用，蹭 CDN / 版本 / 发现机制。本仓库的 `registry/` 就是：

```
registry/
├── agents/            # 可分发的单个 Agent（当前只收 kind=skill / kind=json，纯文本免编译）
│   └── brief/
│       ├── agent.yaml
│       └── SKILL.md
└── hivefiles/         # 成品 Room 编排
    └── skill-demo/
        └── Hivefile.yaml
```

三种 URL 写法都能识别（CLI / Hivefile 的 `image:` 字段都支持）：

```bash
hive hire my-room github://xxx1766/Hive-/registry/agents/brief          # 1. scheme 形式
hive hire my-room https://github.com/xxx1766/Hive-/tree/main/registry/agents/brief   # 2. 浏览器 URL
hive hire my-room xxx1766/Hive-#registry/agents/brief@v0.1.0            # 3. 短格式（类 go-get）
```

`@<ref>` 可选（tag / branch / commit sha），缺省 `main`。

`hive up` 同样支持远端 Hivefile，且 Hivefile 里 `agents:` 列表里的每一项也可以是远端 ref：

```bash
hive up github://xxx1766/Hive-/registry/hivefiles/skill-demo
```

**kind=binary 不支持远端拉取**（编译产物跨平台 + 信任模型太重）；想分发 binary Agent 仍需要用户本地 `hive build`。

**安全提示**：拉的是别人仓库里的 SKILL.md，会在你本地 sandbox 里驱动 LLM。Hive 的 Rank + namespace 做了兜底，但仍建议固定 `@<commit-sha>` 避免别人事后篡改 main。

详见 [`registry/README.md`](registry/README.md)。

## Volume & 跨 Room 共享记忆

默认 Room 之间 **什么都不共享**（这是"跨 Room 隔离"卖点的前提）。要让 Agent 把知识/缓存/事实**持久化**到可以让其他 Room 读到的位置，用 **Volume**：

```bash
hive volume create kb        # ~/.hive/volumes/kb/ 创出来
hive volume ls               # 所有 Volume
hive volume rm kb            # 连着内容一起删
```

Agent 通过 **memory/\*** API 读写（SDK `a.MemoryPut/Get/List/Delete`，或者 runner 里的 `memory_put/get/list/delete` 工具）。`scope` 字段决定落在哪：

| `scope` | 落在哪 | 跨 Room 可见？ |
|---|---|---|
| `""`（空字符串） | `~/.hive/rooms/<roomID>/memory/` | ❌ Room 私有，daemon 重启仍在 |
| `"<volume-name>"` | `~/.hive/volumes/<name>/memory/` | ✅ 所有 Room 共读共写 |

**访问控制**：Rank 的 `MemoryAllowed`（staff 起）是 binary gate —— 要用 memory/\* 至少 staff；`scope` 本身目前不做 Hivefile-level ACL，知道 volume 名的都能访问，适合"信任同一批 Agent 作者"的场景。

**一致性**：文件每 key 一个 + 原子 rename，弱一致就绪；不做 lease / CAS（用户反馈的需求场景是"几轮才有要记的要点"，不用加锁）。强一致 v2 再说。

**示例**：`examples/memo/`（静态 workflow Agent，`memory_put` + `memory_list` 两步）；`scripts/demo.sh` 场景 11 演示两个 Room 读写同一 volume。

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
├── hived/                hived daemon
├── hive-skill-runner/    内置 runner (kind=skill Agent 的 entry binary)
└── hive-workflow-runner/ 内置 runner (kind=workflow Agent 的 entry binary)

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
├── summarize/            staff rank，演示 llm/complete + token 配额
├── brief/                staff rank，kind: skill —— 只有 SKILL.md + agent.yaml
├── url-summary/          staff rank，kind: workflow（静态）—— agent.yaml + flow.json
├── research/             staff rank，kind: workflow（LLM 规划）—— agent.yaml + PLANNER.md
└── memo/                 staff rank，kind: workflow —— 演示 memory_put/list 读写共享 Volume

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

- [x] ~~**Hivefile `quota:` override**~~ —— 已生效。Hivefile `agents[].quota` 和 `hive hire --quota '<json>'` 都会合并到 rank 默认上（key 粒度替换）。
- [x] ~~**`capabilities.requires` 校验**~~ —— 已生效。hire 时 manifest `requires:` 会和 Rank `Capabilities()` 对照，不匹配返回 `rank_violation`。`provides:` 目前仅用于声明，未来可做 discovery。
- [x] ~~**`hive hire --quota`**~~ —— 已支持，`--quota '<json>'` flag。
- [x] ~~**`hive logs <room>`**~~ —— 已实现。`hive logs <room>` 一次性 dump 所有 Agent 的 stderr；`hive logs <room> <agent>` 筛一个。无 tail/follow（用 `tail -f` 直接读文件）。
- [ ] **`hive up` 不支持 `--room <name>` 覆盖**：演示多 Room 要备多份 Hivefile。加一个 `--room` 标志。
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

新增（来自 `ARCHITECTURE.md` §"架构扩展方向"）：

- [ ] **`mcp/call` proxy**：Hive 作为 MCP 客户端调外部 MCP server。新建 `internal/proxy/mcpproxy/`，支持 stdio + HTTP 两种 transport；Rank 加 `MCPAllowed` 字段；配额 key `api_calls:mcp:<server>`。
- [ ] **`ai_tool/invoke` proxy**：Agent 通过 `exec` 方式调 Claude Code CLI / Cursor CLI。新建 `internal/proxy/aitoolproxy/`；Rank 加 `AIToolAllowed`；配额 key `api_calls:ai_tool:<name>`；注意会话/上下文持久化语义。
- [x] ~~**`manifest.kind` 字段**~~ —— 已完成：`internal/image/manifest.go` 加了 `Kind` / `Skill` / `Model` / `Tools` 字段；`internal/daemon/daemon.go:handleAgentHire` 检测 Kind 并走 `prepareSkillImage` 分支。
- [x] ~~**`kind: skill` Agent 形态**~~ —— 已完成：`cmd/hive-skill-runner/` 二进制 + ReAct-lite JSON 循环；`examples/brief/` 作为参考 skill Agent。
- [x] ~~**`kind: workflow` Agent 形态**~~ —— 已完成：`cmd/hive-workflow-runner` 支持两种模式：`workflow: flow.json` 静态声明 + `planner: PLANNER.md` LLM 规划；变量替换 `$input.x` / `$steps.<id>.<path>`；`examples/{url-summary,research}/` 两个参考实现。

### 🚀 v2（`DEMO_PLAN.md` 里明确列为"不做"的大特性）

- [ ] **seccomp-bpf syscall 白名单**：生产级沙箱补强，防止内核漏洞提权。
- [ ] **user namespace + uid remap**：脱离 root 运行 daemon。
- [ ] **OCI-style 层状镜像**：取代当前的"复制整个目录"策略，支持层缓存、内容寻址、digest 校验。
- [x] ~~**远端 Registry（`hive pull`）**~~ —— MVP 简化版已完成：GitHub 公开目录作 registry，`hive hire` / `hive up` 接受三种 URL 形式；详见 `registry/README.md`。真正的"独立 Registry 服务 + hive push"仍在 v2。
- [x] ~~**跨 Room 持久化记忆（共享 KV）**~~ —— 已完成：`hive volume create`、`memory/*` API、弱一致语义。见 §Volume & 跨 Room 共享记忆。
- [ ] **跨 Room 实时通信**：Room A Agent 给 Room B Agent 发消息（等价于 docker networks，不是持久化）。
- [ ] **Volume filesystem mount**：在 Hivefile `volumes:` 里声明 ro/rw 挂载点，agent 能直接 fs_read/fs_write 读写；memory/* 依旧可用。
- [ ] **跨主机 / 多 daemon 集群**：一个 CLI 连多台机器的 hived（类似 docker swarm）。
- [ ] **非 Linux 支持**：macOS（用 macOS Virtualization.framework？）/ Windows（WSL2？）。
- [ ] **Hivefile 嵌套**：一个 Hive 可以 hire 另一个 Hive（函数调用式）。
- [ ] **`kind: script` Agent 形态**（Python / Node / Bash）：涉及解释器依赖管理 —— venv / node_modules 谁维护、Image 体积、解释器 bind-mount 策略。

### 📚 文档

- [ ] Agent 作者指南（从零写一个 Agent 的 step-by-step）
- [ ] JSON-RPC 方法完整参考（目前零散在 `internal/rpc/`）
- [ ] 非 Go 语言 SDK 写法示例（Python 最小实现）
- [ ] ARCHITECTURE.md 里 §113（Rank 六类权限）与代码里 `rank.Rank` 的映射对照

---

## 许可证

[MIT](LICENSE)。
