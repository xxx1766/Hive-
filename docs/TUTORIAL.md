# Hive 上手教程

从装好到跑起一个自己写的 Agent，大概 15 分钟。

前置：Linux、Go 1.22+、一台有 `sudo` 权限的机器（hived 要开 namespace）、Python 3（仅下面某些可选步骤需要）。

---

## 1. 安装

从仓库根目录：

```bash
sudo ./scripts/install.sh
```

脚本做三件事：

1. 检查 Go 版本
2. `make build` 编出 4 个二进制
3. 拷到 `/usr/local/bin/`（`hive` / `hived` / `hive-skill-runner` / `hive-workflow-runner`），顺手 `mkdir -p ~/.hive/{images,rooms}`

### 不想 sudo

```bash
PREFIX=$HOME/.local ./scripts/install.sh
export PATH=$HOME/.local/bin:$PATH   # 加到 ~/.bashrc / ~/.zshrc
```

> 即便 binary 装在 user-local，`hived` 启动本身**仍然需要 root**（因为要 `CLONE_NEWNS` / `CLONE_NEWNET`）。把 `sudo hived` 当成和 `sudo dockerd` 一样的东西。

### 跳过编译直接装

```bash
./scripts/install.sh --skip-build      # 当 bin/ 已经有编好的二进制时用
```

---

## 2. 起 daemon

```bash
sudo hived &
hive version        # 两行：hive + hived
hive ping           # pong
```

默认 daemon 监听 `$XDG_RUNTIME_DIR/hive/hived.sock`，没 `$XDG_RUNTIME_DIR` 就用 `~/.hive/hived.sock`。

### 开发环境不想开 namespace

```bash
HIVE_NO_SANDBOX=1 hived &    # 不需要 root 了，但 Room 之间不再内核级隔离
```

仅 dev / CI 用。

---

## 3. 跑第一个 Agent（从 GitHub registry）

`xxx1766/Hive-/registry/agents/brief` 是一个 20 行的 markdown skill Agent，做"把一段话总结成一句"。

```bash
ROOM=$(hive init demo)
echo "room: $ROOM"

# 拉到本地 store（daemon 走 raw.githubusercontent.com）
hive pull github://xxx1766/Hive-/registry/agents/brief

# hire + run
hive hire "$ROOM" brief:0.1.0
hive run  "$ROOM" '{"text":"Hive 是一套面向多 Agent AI 的能力级虚拟化系统，类比 Docker for Agents。"}'
```

**说明**：

- 没有 `OPENAI_API_KEY` 时 daemon 内置 `mock` provider —— 输出会是 `mock-summary: ...`，流程仍能跑完，适合离线 / CI 验证
- 想用真 LLM：`sudo kill $(pgrep hived); OPENAI_API_KEY=sk-... sudo -E hived &`

### 观察状态

```bash
hive team  "$ROOM"              # Agent 列表 + 剩余配额
hive rooms                       # 所有 Room
hive logs  "$ROOM"               # 聚合 stderr；`hive logs $ROOM brief` 筛一个
hive stop  "$ROOM"               # 收工
```

---

## 4. 远端拉取的三种 URL 形式

`hive hire` / `hive up` / `hive pull` 都支持：

| 形式 | 样例 |
|---|---|
| scheme | `github://xxx1766/Hive-/registry/agents/brief` |
| 浏览器 URL | `https://github.com/xxx1766/Hive-/tree/main/registry/agents/brief` |
| 短格式 | `xxx1766/Hive-#registry/agents/brief@v0.1.0` |

`@ref` 可选，默认 `main`；可以是 branch / tag / 完整 commit SHA。生产环境**强烈建议固定到 SHA**，避免 owner 事后改 main 偷换内容。

---

## 5. 写一个自己的 skill Agent

三种形态可选，最快的是 `kind: skill`：

```
my-summarizer/
├── agent.yaml
└── SKILL.md
```

`my-summarizer/agent.yaml`：

```yaml
name: my-summarizer
version: 0.1.0
kind: skill
skill: SKILL.md
model: gpt-4o-mini
rank: staff           # staff 给 net + llm + /data 读写
tools: [net, llm]
capabilities:
  requires: [llm]
  provides: [summarize]
quota:
  tokens:
    "gpt-4o-mini": 2000
```

`my-summarizer/SKILL.md`（就是喂给 LLM 的 system prompt）：

```markdown
你是一个摘要助手。输入是 `{"text": "原文"}`，输出一句不超过 30 字的总结。

严格按 Hive runtime 协议返回：
- 完成时：`{"answer": "你的总结"}`
- 需要调 tool 时：`{"tool": "<name>", "args": {...}}`

本 skill 不需要调 tool，直接出 answer。
```

构建 + 运行：

```bash
hive build ./my-summarizer
hive hire "$ROOM" my-summarizer:0.1.0
hive run  "$ROOM" --target my-summarizer '{"text":"..."}'
```

### 另外两种形态

- **`kind: workflow`（静态）**：把步骤用 `flow.json` 写死，runner 顺序执行，支持 `$input.x` / `$steps.<id>.y` 变量替换。参考 `examples/url-summary/`。
- **`kind: workflow`（LLM 规划）**：写一份 `PLANNER.md` 告诉 LLM 怎么拆任务，runner 让 LLM 一次性产出 `flow.json` 再 deterministic 执行。参考 `examples/research/`。
- **`kind: binary`（Go / 任意语言）**：最灵活，但要自己编译。参考 `examples/fetch/`、`examples/summarize/`，Go SDK 在 `sdk/go/`。

详细对比在 [`README.md`](../README.md#写一个自己的-agent)。

---

## 6. 多 Agent 组队：Hivefile

把要招的 Agent 写到一个 YAML 里，一键拉起：

`hivefiles/my-team.yaml`：

```yaml
room: research-project
entry: my-summarizer              # hive run 默认的收件人
agents:
  - image: github://xxx1766/Hive-/registry/agents/brief
    rank: staff
  - image: my-summarizer:0.1.0    # 本地 image
    rank: staff
    quota:                         # 覆盖默认配额
      tokens:
        "gpt-4o-mini": 500
```

```bash
ROOM=$(hive up hivefiles/my-team.yaml)
hive team "$ROOM"
hive run  "$ROOM" '{"text":"..."}'
```

想让同一份 Hivefile 起多个独立的 Room（跑并行演示 / 实验），加 `--room <name>` 覆盖即可：

```bash
ROOM_A=$(hive up hivefiles/my-team.yaml --room demo-a)
ROOM_B=$(hive up hivefiles/my-team.yaml --room demo-b)
```

`hive up` 本身也可以吃一个远端 URL：

```bash
ROOM=$(hive up github://xxx1766/Hive-/registry/hivefiles/skill-demo)
```

里面 `agents[].image` 是远端时，daemon 逐个 pull 到本地再 hire。

---

## 7. 跨 Room 共享记忆（Volume + memory/\*）

到目前为止两个 Room 互不可见。如果你想让 Agent A 学到的东西被 Agent B 重用（跨 Room 的 KV 知识库、缓存、事实表），用 **Volume + memory API**：

```bash
# 建一个命名的持久化容器
hive volume create kb

# Room 1 写
ROOM1=$(hive init worker-1)
hive hire "$ROOM1" memo:0.1.0                  # 先 hive build ./examples/memo
hive run  "$ROOM1" '{"scope":"kb","key":"gpt4o:ctx","value":"128k"}'

# Room 2 读同一个 kb —— 看得到 Room 1 的写
ROOM2=$(hive init worker-2)
hive hire "$ROOM2" memo:0.1.0
hive run  "$ROOM2" '{"scope":"kb","key":"claude:ctx","value":"200k"}'
# output 里的 keys 会列出 ["gpt4o:ctx", "claude:ctx"] —— 两个 Room 的写都可见

# 私有 scope（空字符串）只在自己 Room 里持久化，daemon 重启仍在但跨 Room 不通
hive run  "$ROOM1" '{"scope":"","key":"my-secret","value":"only-room-1"}'
```

**要点**：

- `scope: ""` = Room-private；`scope: "<volume>"` = 跨 Room 共享
- Rank 要 staff 起（intern 没 `MemoryAllowed`）
- 自己写代码用：`sdk/go` 的 `a.MemoryPut / Get / List / Delete`
- skill / workflow agent 用：`memory_put` / `memory_get` / `memory_list` / `memory_delete` 工具，manifest `tools: [memory]`

完整设计在 [`../README.md`](../README.md#volume--跨-room-共享记忆) 的 §Volume 节。

### 文件级共享（fs_read / fs_write）

有些场景（写大文件、图片、PDF、或者一堆文件的目录）不适合走 KV，直接拿文件系统更顺手。Volume 可以被 bind-mount 进 Agent 的 sandbox：

```bash
# 一次性（CLI）
hive hire my-room my-agent:0.1.0 --volume kb:/shared/kb:rw
```

```yaml
# Hivefile
agents:
  - image: my-agent:0.1.0
    volumes:
      - {name: kb, mountpoint: /shared/kb, mode: rw}
```

Agent 直接 `fs_write("/shared/kb/paper.pdf", ...)`，daemon 把路径重定向到 `~/.hive/volumes/kb/paper.pdf`。两个 Room 挂同一个 volume 就能互相看到对方写的文件。Rank 的 FSRead/FSWrite 自动扩 mountpoint，不用单独声明。

参考：`examples/blob/`；`scripts/demo.sh` 场景 12。

## 8. 让 Agent 调用 Claude Code

Hive 的 "Docker for Agents" 定位意味着 Agent 可以把 **外部 AI 工具当算力后端**。第一个落地的是 Claude Code CLI —— Agent 可以通过 `ai_tool/invoke` 把一段 prompt 扔过去、让 claude 在 **Room 专属的 workspace 目录** 里读/写文件、然后把结果取回来。

### 前提

- 宿主机装好 `claude` CLI（比如 `npm i -g @anthropic-ai/claude-code` 或等效）
- daemon 启动时 `ANTHROPIC_API_KEY` 在环境里：
  ```bash
  export ANTHROPIC_API_KEY=sk-ant-...
  sudo -E hived &     # -E 保留环境变量
  ```
- 没 API key 时 daemon 自动挑 **MockProvider**（返回 `mock-claude: <prompt>`），integration 测试和 demo 离线可跑

### 每个 Room 自带 workspace

Hive 给每个 Room 建一个 `~/.hive/rooms/<id>/workspace/` 子目录，并在 Agent 被 hire 时**无条件**以 rw 方式 bind-mount 到沙箱里的 `/workspace`。

- Agent 侧：`fs_write("/workspace/foo.go", ...)` 把输入喂给 claude；`fs_read("/workspace/foo.go")` 把 claude 改完的文件读回来
- daemon 侧：`claude -p <prompt>` 的 cwd 就是 `~/.hive/rooms/<id>/workspace/`
- 这样 claude 的默认工具（Read / Edit / Write）**天然只看得到 workspace 里的文件** —— 教程末尾的"安全边界"一节聊更细的限制

### 最小 Agent：`examples/coder/`

```yaml
# agent.yaml
name: coder
version: 0.1.0
kind: workflow
workflow: flow.json
rank: staff
tools: [fs, ai_tool]
capabilities:
  requires: [fs, ai_tool]
  provides: [coder]
```

```json
// flow.json
{
  "steps": [
    {"id":"stage",    "tool":"fs_write",       "args":{"path":"$input.filename","content":"$input.code"}},
    {"id":"refactor", "tool":"ai_tool_invoke", "args":{"tool":"claude-code","prompt":"$input.prompt"}},
    {"id":"read_back","tool":"fs_read",        "args":{"path":"$input.filename"}}
  ],
  "output": "$steps.read_back"
}
```

### 跑一下

```bash
hive build ./examples/coder
ROOM=$(hive init coder-test)
hive hire "$ROOM" coder:0.1.0
hive run "$ROOM" '{
  "filename": "/workspace/hello.go",
  "code":     "package main\nfunc main(){println(\"hi\")}\n",
  "prompt":   "add a TODO comment on the main function"
}'
```

输出里：
- `output` 字段 = 读回来的文件内容（如果 claude 真修改了，这里就是新版本）
- `steps.refactor.output` = claude 的 stdout
- workspace 里的 `hello.go` 文件现场还在（`hive logs $ROOM` 也能看）

### Rank 和配额

- `intern` 没 `AIToolAllowed`，`hive hire ... --rank intern` 会在 hire 时就拒绝（`rank_violation`）
- `staff` 默认 `ai_tool:claude-code` quota = 10 次/Agent；超了报 `quota_exceeded`
- `manager` = 100，`director` 无限；可以在 Hivefile 或 `hive hire --quota '{...}'` 里覆盖

### 安全边界（soft confinement）

**软限制**：
- Claude Code 进程的 **cwd 被锁在 workspace** —— 默认的 Read/Edit/Write 操作不会越界
- Rank 的 `AIToolAllowed` + 每次调用的 quota 是 **硬约束**，由 daemon 在 Hive 层 enforce

**软限制里漏的地方**：
- Claude Code 的 Bash tool 可以跑 `cat /etc/passwd`、`ls /` —— 这条目前没拦
- 真要"硬沙箱"（`firejail --private=<workspace>` / `bwrap` / user namespace 包一层），是 README TODO 的 v2 项，等你需要时再开
- 现阶段建议：只把 `coder` 这类 Agent 给信任的场景用，或者把 daemon 用 non-root user + `sudo setcap` 方式跑来缩小 blast radius

### 其他 AI 工具

实现在 `internal/proxy/aitoolproxy/` 的 Provider interface 是可插的：

```go
type Provider interface {
    Name() string
    Invoke(ctx, cwd, prompt string, timeout time.Duration) (Result, error)
}
```

想接 Cursor CLI / Codex / 任何 cwd-based CLI AI 工具，加一个 Provider 就行。`ai_tool/invoke` 的 `tool` 字段选 provider 名字，其他什么都不变。

## 9. 发布你的 Agent 到公共 Registry

直接往本仓库发 PR 到 `registry/agents/<your-agent>/` 即可（MVP 简化方案）。未来会拆到独立 repo。

---

## 10. 排障

| 现象 | 可能原因 |
|---|---|
| `hive: cannot connect to hived` | daemon 没起，或 socket 路径不对（检查 `$HIVE_SOCKET`） |
| `rank "intern" does not grant required capability "llm"` | Agent 的 manifest `requires: [llm]` 但 rank 不给，换 `--rank staff` |
| `http quota exhausted` | 配额用完了；`hive team` 看剩余，`--quota '<json>'` 或 Hivefile 覆盖 |
| `agent exited` 无详细信息 | namespace init 可能崩了；`HIVE_NO_SANDBOX=1` 重启 daemon 排除沙箱问题 |
| Agent 输出是 `mock-summary: ...` | daemon 没看到 `OPENAI_API_KEY`，走了内置 mock provider |

---

## 11. 卸载

```bash
sudo ./scripts/uninstall.sh          # 只删二进制，保留 ~/.hive
sudo ./scripts/uninstall.sh --purge  # 连 ~/.hive 一起删
```

---

## 12. 继续深入

- **命令完整参考** → [`../README.md`](../README.md) 的 §命令速查
- **架构设计 + 术语表** → [`../ARCHITECTURE.md`](../ARCHITECTURE.md)
- **Rank 权限模型** → `README.md` §Rank
- **写非 Go 语言 SDK** → 直接读写 stdin/stdout 的 JSON-RPC 2.0，参考 `examples/echo/main.go`
- **当前还没做的** → `README.md` 底部 TODO backlog
