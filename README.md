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
git clone https://github.com/xxx1766/Hive-.git
cd Hive-
sudo ./scripts/install.sh          # 或 PREFIX=$HOME/.local 装到用户目录
```

脚本会检查 Go 版本、`make build`、把 4 个 binary 装进 `/usr/local/bin/`、初始化 `~/.hive/`。

> 后续要升级到最新代码：
> - 已经装过 `hive` → `sudo hive update`（详见 `hive help update`）
> - 还没装 / binary 坏了 → `sudo ./scripts/update.sh` 或 `sudo make update`（同等效果，纯 shell，不依赖现有 binary）

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
./bin/hive agents

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
| `hive build <dir>` | 把一个 Agent 源码目录打包为 Hive Image（"Hive Image" 是底层打包概念，CLI 里就叫 Agent） |
| `hive agents` | 列出本地已装好的 Agent |
| `hive init <name>` | 创建新 Room，返回 RoomID |
| `hive rooms` | 列出所有 Room |
| `hive hire <room> <ref>` | 把单个 Agent 招进已有 Room；`<ref>` 可以是 `name:version`（本地）或远端 URL（三种形式见 §Registry） |
| `hive hire -f <hivefile\|url> [--room <name>]` | 批量模式：按 Hivefile 一次性建 Room + 招聘所有声明的 Agent；`--room` 可覆盖 Hivefile 里的 room 名；hivefile 本身和里面的 Agent 都可远端 |
| `hive pull <url>` | 显式把一个远端 Agent 拉到本地 store |
| `hive team <room>` | 列出 Room 内 Agent 及配额剩余 |
| `hive volume create/ls/rm` | 管理跨 Room 持久化卷 |
| `hive run <room> [task]` | 下发任务，实时流式打印 Agent 日志（`--target <image>` 选收件人） |
| `hive stop <room>` | 停掉 Room |
| `hive update` | 拉最新 hive 源码、重 build、重装（`--check` 只看不装） |

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
| Hive → Agent | `task/run` | 下发任务（带可选 `conv_id` 把任务绑到一个 Conversation） |
| Hive → Agent | `peer/recv` | 同 Room peer 发来的消息（带 `conv_id` 时触发 round 计数） |
| Hive → Agent | `shutdown` | 温和终止 |
| Agent → Hive | `fs/read` `fs/write` `fs/list` | 受 Rank 约束的文件 I/O |
| Agent → Hive | `net/fetch` | HTTP 请求（扣 `api_calls` 配额） |
| Agent → Hive | `llm/complete` | LLM 调用（扣 token 配额） |
| Agent → Hive | `peer/send` | 给同 Room 的指定 Agent 发消息（fire-and-forget） |
| Agent → Hive | `hire/junior` | manager+ Rank：运行时招聘下属 Agent，配额从自身 carve（见 §多 Agent 协作） |
| Agent → Hive | `task/done` `task/error` | 任务终态 |
| Agent → Hive | `log` | 结构化日志 |

---

## Rank（权限 + 配额职级）

内置四档：

| Rank | 文件系统 | 网络 | LLM | Memory | AI Tool | 默认配额 |
|---|---|---|---|---|---|---|
| `intern` | 读 `/app` `/tmp`；写 `/tmp` | ✓ | ✗ | ✗ | ✗ | http=5 |
| `staff` | 读 `/app` `/tmp` `/data`；写 `/tmp` `/data` | ✓ | ✓ | ✓ | ✓ | http=20, tokens(gpt-4o-mini)=5000, ai_tool:claude-code=10 |
| `manager` | 读 `/`；写 `/tmp` `/data` | ✓ | ✓ | ✓ | ✓ | http=200, tokens=50000, ai_tool:claude-code=100 |
| `director` | 全权限 | ✓ | ✓ | ✓ | ✓ | 无限 |

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

`hive hire -f` 同样支持远端 Hivefile，且 Hivefile 里 `agents:` 列表里的每一项也可以是远端 ref：

```bash
hive hire -f github://xxx1766/Hive-/registry/hivefiles/skill-demo
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

### Volume 的文件系统挂载

除了走 memory/\* API，Volume 也可以被**bind-mount 进 Agent 的 sandbox**，直接用 `fs_read` / `fs_write` 读写。适合"写二进制 blob"、"一批文件"这种不好塞进 KV 的场景。

```bash
# CLI 方式
hive hire my-room my-agent:0.1.0 --volume kb:/shared/kb:rw
```

```yaml
# Hivefile 方式
agents:
  - image: my-agent:0.1.0
    rank: staff
    volumes:
      - {name: kb, mountpoint: /shared/kb, mode: rw}
      - {name: assets, mountpoint: /shared/assets, mode: ro}
```

Agent 内部用 `fs_write("/shared/kb/paper.pdf", ...)` 写、`fs_read("/shared/kb/paper.pdf")` 读。daemon 侧 fsproxy 知道 mount 重定向，把 agent path 映射到 `~/.hive/volumes/<name>/<rel>` —— 两个 Room 挂同一个 volume 时互相能看到对方写的文件。

权限：Rank 的 FSRead/FSWrite 会在 hire 时**自动扩出挂载点**（rw 加到 FSWrite，ro 只加到 FSRead），不用手动在 rank 里声明 `/shared/*`。

参考实现：`examples/blob/`（把任意 path+content 写进 mounted volume，再 list 回来）；`scripts/demo.sh` 场景 12 演示两个 Room 通过 fs mount 交换文件。

## 多 Agent 协作（Conversation / hire_junior / peer_call）

Hive 的产品定位是 **"多 Agent 分工协作"** —— 这一节是把那句话从口号变成代码。三件配套：

### 1. Conversation —— 多轮 transcript + 轮数上限

`peer/send` 是 fire-and-forget 的底层消息 IPC，但裸用没有"任务从开始到结束"这个上层概念，agent 跑飞也没有外力能拦住。Conversation 把零散 peer 消息组成持久化的 task transcript，daemon 端强制 `max_rounds`（默认 8），任何方向的 hop 都计一轮，超就 status=cancelled，理由 `round_cap`。

```bash
# 通过 IPC / HTTP 创建 + 启动一个 Conversation
curl -s -X POST http://127.0.0.1:8910/api/rooms/$ROOM/conversations \
  -H 'Content-Type: application/json' \
  -d '{"target":"paper-coordinator","input":{"section":"design"},"max_rounds":12}'
# → {"conv_id":"conv-…","status":"planned"}

curl -s -X POST http://127.0.0.1:8910/api/rooms/$ROOM/conversations/$CID/start
# 之后看 SSE 实时事件
curl -N http://127.0.0.1:8910/api/rooms/$ROOM/events
```

每个 Conversation 一份 JSON 文件，落在 `<RoomsDir>/<roomID>/conversations/<convID>.json`，原子 temp+rename 写入，daemon 重启时 `active` → `interrupted`（旧进程已死，没人能续）。

### 2. hire_junior —— manager+ 运行时招聘下属

manager+ Rank 的 Agent 在 ReAct 循环里能调 SDK `hive.HireJunior(ref, rank, opts)`：

- `rank.CanHire` 三规则：调用方必须 `HireAllowed=true`（默认仅 manager + director）；child rank 必须**严格小于** self（manager 招 staff/intern，不能招另一个 manager —— 防 peer-cycle）；同 Room 内 image 名唯一。
- **配额 carve + refund-on-exit**：每个 token / api_call 桶都通过 `quota.Consume` 原子地从 parent 扣减；任一桶不够，整个 hire 失败。子 agent 退出时通过 `quota.Uncharge` 自动把没用完的余量回流给 parent —— supervisor 给 critic carve 8k，critic 用了 3k，5k 在 critic 退出后回到 supervisor 桶里，可在 `hive team` 看见。
- **Subordinate tree**：`Member.Parent` + `roomstate.MemberSnap.Parent` 持久化树形结构。daemon 重启后整树原样恢复。HTTP UI Team tab 用 `└─ paper-writer (hired by paper-coordinator)` 这种 indent + 注解直观渲染。

### 3. peer_call —— 同步等回复，让委派结果真回到 transcript

`peer_send` 是 fire-and-forget；coordinator 委派给 worker 后立刻返回，conversation flips done，worker 的回复就被 `PeerSendIntercept` 拒掉。`peer_call`（仅 skill-runner 支持）补上同步语义：

```
1. 注册 awaiter (target, conv_id)   ← 在 send 前
2. PeerSend 出去
3. 阻塞读 awaiter 的 channel        ← peer-router goroutine 自动路由
4. 把对方 reply payload 当 tool result 返还给 LLM
```

LLM 用 `{"tool":"peer_call","args":{"to":"paper-writer","payload":{"section":"design"},"timeout_seconds":120}}` 调；默认 60s 超时，可调到 300s。

### 端到端 Demo：paper-coordinator

`examples/paper-assistant/coordinator/` 是个 manager-rank skill agent，演示三件套全用：

1. 接到 `{"section":"design"}` 任务
2. `hire_junior` 现场招个 paper-writer (staff)，carve 30k tokens
3. `peer_call` 把任务转给 writer 同步等回复
4. 把 writer 的产出报告 (`design.md written, ~670 words`) weave 进 final answer

跑起来：
```bash
./bin/hive build ./examples/paper-assistant/coordinator
./bin/hive build ./examples/paper-assistant/writer
./bin/hive volume create paper-osdi-corpus paper-osdi-draft
cp examples/paper-assistant-osdi/sample-corpus/*.md ~/.hive/volumes/paper-osdi-corpus/
ROOM=$(./bin/hive hire -f hivefiles/paper-assistant/coordinator-demo.yaml)
# 浏览器开 http://127.0.0.1:8910，"+ New Conversation"，target=paper-coordinator，input={"section":"design"}
```

UI 上能看到完整的 4 条 transcript：task_input → peer (round 1，coord→writer) → peer (round 2，writer→coord) → task_output；Team tab 的 subordinate tree；Volumes tab 里的 design.md。完整 walkthrough 见 `examples/paper-assistant/coordinator/README.md`。

详细架构 + v2 路线图见 `ARCHITECTURE.md` §"Conversation 与多轮协作" 和 §"Auto-hire 与配额 carve"。

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
├── memo/                 staff rank，kind: workflow —— 演示 memory_put/list 读写共享 Volume
└── blob/                 staff rank，kind: workflow —— 演示 fs_write/list 读写 bind-mount 的 Volume

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
- [x] ~~**`hive hire -f --room <name>` 覆盖**~~ —— 已支持。同一份 Hivefile 可以用 `hive hire -f hivefile.yaml --room demo-a` / `--room demo-b` 跑出并行的独立 Room。
- [x] ~~**demo.sh 的 `set -o pipefail` 坑**~~ —— 抽了个 `run_tolerant` helper（`scripts/demo.sh`），把"明知可能失败（quota 拒绝 / 远端拉取 / 负断言）"的调用包起来，`out=$(run_tolerant ...)` 就不会再被 pipefail 或 `set -e` 误伤。
- [x] ~~**Agent 崩溃的诊断信息难回传**~~ —— `ns.NewAgentCommand` 现在多开了一条 init-err 管道（父进程读端，子进程 FD 3）。`RunInit` 在 setup 失败时把错误同时写到 stderr log 和 FD 3，成功时在 `syscall.Exec` 之前把 FD 3 关掉；`agent.Conn.WaitInit` 阻塞到这一信号再让 `room.Hire` 返回，于是 `hive hire`（含 `-f`）看到的是 `sandbox init: setup: pivot_root: operation not permitted` 而不再是笼统的 `agent exited`。
- [x] ~~**Agent 日志没 rotation**~~ —— 新增 `internal/daemon/logrotate.go`：每个 Agent 的 `*.stderr.log` 默认 10 MiB 上限，到顶就 rename 成 `.1` + 重开新文件，只保留一份 backup（"不爆盘"重于"留全量"）。可用 `HIVE_LOG_MAX_BYTES` 覆盖。顺带修了一个 FD 泄漏：以前 hire 的 log 文件始终不 close，现在 `room.Hire` 在 agent 退出时 close（cmd.Wait 已 join exec 的 stderr copy goroutine，无丢字节 race）。

### 🧱 中期（架构内稳健性）

- [ ] **集成测试（Go）**：目前只有单元测试 + `demo.sh`。补一个 `TestEndToEnd`，exec hived 起来、走 IPC 全链路、断言 `hire → run → team` 结果。
- [ ] **Rank 级 peer 策略**：`room.Hooks.AuthPeerSend` 已经预留，但 demo 里所有同 Room peer 都放行。加个 Rank 粒度的 allow-list（intern 只能给 manager 发消息之类）。
- [ ] **Capabilities 匹配**：`requires` 和 `provides` 在 hire 时真 enforce（例如 Hivefile 里所有 `requires` 必须有对应的 `provides`）。
- [ ] **更多 LLM provider**：现在只有 `mock` 和 `openai`（OpenAI-compatible）。加 `anthropic`、配置驱动的 provider routing。
- [ ] **`hive exec <room> <agent> <cmd>`**：类似 `docker exec`，给运行中的 Agent 注入一次性任务。
- [x] ~~**daemon 重启 Room 持久化**~~ —— 已完成：`internal/roomstate/` 把每个 Room 的 hire 清单序列化到 `state.json`，`recoverRooms` 启动时按清单重新 hire。Conversation 也跟着持久化（`<RoomsDir>/<roomID>/conversations/<convID>.json`），active → interrupted on restart。
- [ ] **`lo` 接口补齐**：`CLONE_NEWNET` 默认 loopback 是 down 的，有些 Agent 内部库会意外失败。init 阶段 `ip link set lo up` 一下（或 Go 语言版的 netlink）。
- [ ] **Agent 输出压缩/分片**：`fs/read` 大文件目前整包 base64 JSON 回传，无流式。加 `fs/read-stream` 或 chunked 语义。

新增（来自 `ARCHITECTURE.md` §"架构扩展方向"）：

- [ ] **`mcp/call` proxy**：Hive 作为 MCP 客户端调外部 MCP server。新建 `internal/proxy/mcpproxy/`，支持 stdio + HTTP 两种 transport；Rank 加 `MCPAllowed` 字段；配额 key `api_calls:mcp:<server>`。
- [x] ~~**`ai_tool/invoke` proxy**~~ —— 已完成：`internal/proxy/aitoolproxy/`、Rank.AIToolAllowed、配额 `api_calls:ai_tool:claude-code`。每 Room 自带 `~/.hive/rooms/<id>/workspace/` 作 claude cwd + bind-mount 作 `/workspace`。`examples/coder/` 和 TUTORIAL §8 有完整示例；Cursor/Codex 等后端加 Provider 即可。硬沙箱（firejail）留 v2。
- [x] ~~**`manifest.kind` 字段**~~ —— 已完成：`internal/image/manifest.go` 加了 `Kind` / `Skill` / `Model` / `Tools` 字段；`internal/daemon/daemon.go:handleAgentHire` 检测 Kind 并走 `prepareSkillImage` 分支。
- [x] ~~**`kind: skill` Agent 形态**~~ —— 已完成：`cmd/hive-skill-runner/` 二进制 + ReAct-lite JSON 循环；`examples/brief/` 作为参考 skill Agent。
- [x] ~~**`kind: workflow` Agent 形态**~~ —— 已完成：`cmd/hive-workflow-runner` 支持两种模式：`workflow: flow.json` 静态声明 + `planner: PLANNER.md` LLM 规划；变量替换 `$input.x` / `$steps.<id>.<path>`；`examples/{url-summary,research}/` 两个参考实现。
- [x] ~~**Conversation primitive（多轮 + 上限）**~~ —— 已完成：`internal/conversation/` Store + Bus；`PeerSendIntercept`/`Delivered` hooks；`max_rounds` 强制；HTTP UI kanban + SSE 实时事件。见 §多 Agent 协作。
- [x] ~~**Auto-hire 下属（manager+ 招 staff/intern）**~~ —— 已完成：SDK `HireJunior`、`hire/junior` IPC、`rank.CanHire` 三规则、原子配额 carve、`Member.Parent` 持久化、UI subordinate tree。见 §多 Agent 协作 + `ARCHITECTURE.md` §"Auto-hire 与配额 carve"。
- [x] ~~**`peer_call` 同步等回复**~~ —— 已完成：skill-runner 加了 peer-router goroutine + awaiter registry；coordinator → worker → coordinator 完整 round-trip 进 transcript。见 `examples/paper-assistant/coordinator/`。
- [x] ~~**HTTP UI**~~ —— 已完成：`internal/httpapi/` embed `index.html`，三栏 kanban + 时间线 + Team 树 + volume 浏览器；SSE 实时推送；默认 `127.0.0.1:8910`，`HIVE_HTTP_ADDR` 可改。

### 🚀 v2（`DEMO_PLAN.md` 里明确列为"不做"的大特性）

- [x] ~~**`hire_junior` refund-on-exit**~~ —— 已完成：`quota.Actor.Uncharge` + 在 `OnAgentExit` 钩子里按 child.EffectiveQuota 把每桶未消耗余量回流到 parent.bucket。可观察：carve 30k 给 sub，sub 用 5k 退出后，parent 的 `hive team` 余量从 -25k 回到 -5k。
- [x] ~~**`peer_call` 在 workflow-runner**~~ —— 已完成：awaiter 抽到 `internal/peerawait/` 共享包；workflow-runner 也有 peer-router goroutine + awaiter，flow.json / planner 可发 `peer_call` / `peer_call_many` 步骤，结果落到 `$steps.<id>.payload`。见 `examples/workflow-peer-call/`（kind: workflow → peer_call → kind: skill 的最小 demo）。
- [x] ~~**跨 Room Conversation**~~ —— 已完成：Conversation.Members 显式声明 (room_id, agent_name) 对；daemon 用 in-memory convIndex 做 room-agnostic 查找；新增 room.Hooks.PeerSendForward 把跨 Room 的 peer/send 转发给目标 home Room 的 router，transcript 仍持久化到单一 owner Room 目录。Demo: `examples/cross-room-demo/`（chatter-a in Room A ↔ chatter-b in Room B，验证 round counter + 双向路由）。
- [ ] **HTTP UI 鉴权**：默认 `127.0.0.1:8910` 只监本地。要远程访问得加 token / mTLS。
- [ ] **HTTP UI hire/fire 控件**：当前 UI 只读，能创建 conversation 但不能直接 hire 新 agent。
- [ ] **seccomp-bpf syscall 白名单**：生产级沙箱补强，防止内核漏洞提权。
- [ ] **user namespace + uid remap**：脱离 root 运行 daemon。
- [ ] **OCI-style 层状镜像**：取代当前的"复制整个目录"策略，支持层缓存、内容寻址、digest 校验。
- [x] ~~**远端 Registry（`hive pull`）**~~ —— MVP 简化版已完成：GitHub 公开目录作 registry，`hive hire` / `hive hire -f` 接受三种 URL 形式；详见 `registry/README.md`。真正的"独立 Registry 服务 + hive push"仍在 v2。
- [x] ~~**跨 Room 持久化记忆（共享 KV）**~~ —— 已完成：`hive volume create`、`memory/*` API、弱一致语义。见 §Volume & 跨 Room 共享记忆。
- [ ] **跨 Room 实时通信**：Room A Agent 给 Room B Agent 发消息（等价于 docker networks，不是持久化）。
- [x] ~~**Volume filesystem mount**~~ —— 已完成：Hivefile `agents[].volumes` 声明 ro/rw 挂载点；`hive hire --volume name:mountpoint:mode` 支持 ad-hoc；fsproxy 自带 mount redirect，写落到真实 volume 目录。
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
