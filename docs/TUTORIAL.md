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

`hive up` 本身也可以吃一个远端 URL：

```bash
ROOM=$(hive up github://xxx1766/Hive-/registry/hivefiles/skill-demo)
```

里面 `agents[].image` 是远端时，daemon 逐个 pull 到本地再 hire。

---

## 7. 发布你的 Agent 到公共 Registry

直接往本仓库发 PR 到 `registry/agents/<your-agent>/` 即可（MVP 简化方案）。未来会拆到独立 repo。

---

## 8. 排障

| 现象 | 可能原因 |
|---|---|
| `hive: cannot connect to hived` | daemon 没起，或 socket 路径不对（检查 `$HIVE_SOCKET`） |
| `rank "intern" does not grant required capability "llm"` | Agent 的 manifest `requires: [llm]` 但 rank 不给，换 `--rank staff` |
| `http quota exhausted` | 配额用完了；`hive team` 看剩余，`--quota '<json>'` 或 Hivefile 覆盖 |
| `agent exited` 无详细信息 | namespace init 可能崩了；`HIVE_NO_SANDBOX=1` 重启 daemon 排除沙箱问题 |
| Agent 输出是 `mock-summary: ...` | daemon 没看到 `OPENAI_API_KEY`，走了内置 mock provider |

---

## 9. 卸载

```bash
sudo ./scripts/uninstall.sh          # 只删二进制，保留 ~/.hive
sudo ./scripts/uninstall.sh --purge  # 连 ~/.hive 一起删
```

---

## 10. 继续深入

- **命令完整参考** → [`../README.md`](../README.md) 的 §命令速查
- **架构设计 + 术语表** → [`../ARCHITECTURE.md`](../ARCHITECTURE.md)
- **Rank 权限模型** → `README.md` §Rank
- **写非 Go 语言 SDK** → 直接读写 stdin/stdout 的 JSON-RPC 2.0，参考 `examples/echo/main.go`
- **当前还没做的** → `README.md` 底部 TODO backlog
