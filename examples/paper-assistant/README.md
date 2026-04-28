# paper-assistant — 论文写作 Agent 团队

4 个专家 Agent 共享一份个人语料、按招聘隐喻协作的论文写作助手。
Hive 的"多 Agent 分工 + Volume 跨 Room 共享 + Rank-as-policy"四件招牌特性的应用级例子。

## TL;DR

```bash
# 跑 demo（60 秒，无需 API key）
bash scripts/paper-demo.sh

# 真用（OSDI 投稿）
cat examples/paper-assistant-osdi/README.md
```

## 4 个 Agent

| 名字 | kind | Rank | 工具 | Volume | 职责 |
|---|---|---|---|---|---|
| `paper-scout` | workflow | staff | net+llm+fs | draft(rw) | net_fetch arxiv → llm 综述 → fs_write `related.md` |
| `paper-outline` | skill | staff | fs+llm | corpus(ro) + draft(rw) | 读风格 + related.md → 产 `outline.md` |
| `paper-writer` | skill | **manager** | fs+llm | corpus(ro) + draft(rw) | 读 outline + 过往论文样本 → 写 `<section>.md`（manager 提供 80–120k token 预算） |
| `paper-reviewer` | skill | staff | fs+llm | corpus(ro) + draft(**ro**) | 应用 anti-pattern checklist；draft 是 ro，**Rank-as-policy 的演示点** |

## 两种用法

### A. Demo（一次性、可重复跑）

`scripts/paper-demo.sh` —— 12 步端到端：build → 起 stub arxiv :8992 → 创 volume → seed corpus → hire ICML team → 跑 scout/outline/writer/reviewer → 起 NeurIPS team 验证 corpus 共享 + draft 隔离 → `hive team` 输出 quota 分布。

### B. 真用（针对实际投稿）

- **OSDI** 见 [`examples/paper-assistant-osdi/README.md`](../paper-assistant-osdi/README.md)（已配 systems 风格 corpus + Caladan/Anna/io_uring stub）
- **ICML / NeurIPS / 其它**：fork `hivefiles/paper-assistant/icml-paper.yaml`，改 `room` 名字 + draft volume 名字，corpus 复用 `paper-corpus`

## 目录

```
examples/paper-assistant/
├── scout/        agent.yaml + flow.json     # workflow agent
├── outline/      agent.yaml + SKILL.md      # skill agent
├── writer/       agent.yaml + SKILL.md      # skill agent
├── reviewer/     agent.yaml + SKILL.md      # skill agent
├── sample-corpus/                           # ML 通用风格语料种子
│   ├── style-notes.md
│   ├── past-paper-methods-1.md
│   └── how-to-write-paper.md
└── sample-arxiv/papers.json                 # 3 篇 attention-sparsity 主题 stub

examples/paper-assistant-osdi/               # OSDI 真用 kit（systems 风格）
├── README.md
├── sample-corpus/{style-notes, past-paper-methods-1, how-to-write-paper-osdi, osdi-reviewer-checklist}.md
└── sample-arxiv/papers.json                 # Caladan / Anna / io_uring

hivefiles/paper-assistant/
├── icml-paper.yaml         # ICML 模板（paper-corpus + paper-icml-draft）
├── neurips-paper.yaml      # NeurIPS 模板（同 corpus，独立 draft）
└── osdi-paper.yaml         # OSDI 模板（paper-osdi-corpus + paper-osdi-draft；提高 quota）

scripts/paper-demo.sh       # 端到端演示
```

## Setup（首次或 `make clean` 后）

```bash
# 1. 编译 daemon
make build

# 2. 启动 daemon（socket 默认在 ~/.hive/hived.sock；环境变量 HIVE_STATE 可换）
./bin/hived >/tmp/hived.log 2>&1 &

# 3. 编译 4 个 agent
for a in scout outline writer reviewer; do
    ./bin/hive build "./examples/paper-assistant/$a"
done

# 4. 创建语料 volume + 第一个项目的 draft volume
./bin/hive volume create paper-corpus
./bin/hive volume create paper-icml-draft     # 或 paper-osdi-draft / paper-<conf>-draft

# 5. 灌库（用 demo 自带的种子，或换成你自己的过往论文 markdown）
HIVE_STATE="${HIVE_STATE:-$HOME/.hive}"
cp examples/paper-assistant/sample-corpus/*.md "$HIVE_STATE/volumes/paper-corpus/"

# 6.（强烈建议）设 LLM provider —— 没这步只能跑通 pipeline，不会产真稿
#    见下面"LLM provider 配置"一节
```

## LLM provider 配置

llmproxy 走 OpenAI 兼容 schema —— 任何 OpenAI-compatible 网关（OpenAI 直连 / 第三方 gateway / 本地推理）都能用，靠两个 env：

| Env | 作用 | 默认 |
|---|---|---|
| `OPENAI_API_KEY` | Bearer token | 必填，缺就退化为 mock |
| `OPENAI_BASE_URL` | API 根 URL（不带 `/chat/completions`） | `https://api.openai.com/v1` |

### OpenAI 直连
```bash
export OPENAI_API_KEY=sk-...
# OPENAI_BASE_URL 不设
```
agent.yaml 默认 `model: gpt-4o-mini`，开箱可用。

### 第三方 gateway（GMI / Together / Groq / DeepSeek / 自建 LiteLLM ...）
```bash
# 例：GMI
export OPENAI_API_KEY="$GMI_API_KEY"
export OPENAI_BASE_URL="https://api.gmi-serving.com/v1"
```
但要留意：你的 gateway 支持的 model 名字可能不是 `gpt-4o-mini`（比如 GMI 用 `openai/gpt-5.4-mini`、`deepseek-ai/DeepSeek-V4-Pro` 这种 `vendor/id` 格式）。Demo 默认 `gpt-4o-mini` 在这种 gateway 上**直接 404**。要换 model 见下一节。

### 换默认 model（不再要 sed）

现在 `hive hire` 直接接受 `--model`，或 Hivefile `model:` 字段，运行时切：

```bash
# 单招路径（最常用）：
ROOM=$(./bin/hive init smoke)
./bin/hive hire "$ROOM" paper-writer:0.1.0 \
    --model openai/gpt-5.4-mini \
    --quota '{"tokens":{"openai/gpt-5.4-mini":80000}}' \
    --no-prompt
# 或省略 --no-prompt，prompt 会问 model? 之后 tokens? 自动问 "for openai/gpt-5.4-mini" 的预算
```

```yaml
# Hivefile 路径：在 agents[i] 下加一行
agents:
  - image: paper-writer:0.1.0
    rank: manager
    model: openai/gpt-5.4-mini      # NEW
    quota:
      tokens:
        "openai/gpt-5.4-mini": 80000
```

**关键**：`--model` 或 Hivefile `model:` 同时设了 `--quota` / `quota:` 时，**记得 quota 的 token key 要跟 model 名字一致** —— 不然 LLM 调用走得通但 quota 不会被记到那个 key 上（在 `hive team` 里也看不见）。Prompt 模式自动帮你对齐；手写 yaml / flag 时要自己注意。

> Workflow agent（如 scout）的 flow.json 如果 hardcode 了 `"model": "..."`，会盖过 `--model` —— 留空 model 字段（runner 会从 HIVE_MODEL fallback）才能让 override 生效。本仓库的 `scout/flow.json` 已删掉 hardcode。

## 每篇 paper 的 session

> 想单招一个 Agent（不走 Hivefile）时：terminal 下 `hive hire <room> <ref>` 会自动进交互模式问 rank / tokens / http / volumes —— Enter 跳过单条、Ctrl-D 跳过其余，加 `--no-prompt` 关掉。脚本里跑（pipe / cron）自动不问。Hivefile 模式（`-f`）从不问。

```bash
# 1. 起 Room（按你的会议 fork hivefile）
ROOM=$(./bin/hive hire -f hivefiles/paper-assistant/icml-paper.yaml)
echo "$ROOM"

# 2. 文献综述（先把 sample-arxiv/papers.json 换成真 arxiv 抓的数据，或起本地 stub server）
./bin/hive run "$ROOM" --target paper-scout    '{"topic":"<your topic>"}'

# 3. 大纲
./bin/hive run "$ROOM" --target paper-outline  '{"hypothesis":"<one-line hypothesis>"}'

# 4. 起草章节（多次跑，传不同 section）
./bin/hive run "$ROOM" --target paper-writer   '{"section":"methods"}'   # ML
./bin/hive run "$ROOM" --target paper-writer   '{"section":"design"}'    # OSDI
./bin/hive run "$ROOM" --target paper-writer   '{"section":"intro"}'

# 5. 自审
./bin/hive run "$ROOM" --target paper-reviewer '{"section":"methods"}'

# 6. 看进度（剩多少 quota / agent 状态）
./bin/hive team "$ROOM"

# 7. （结束）
./bin/hive stop "$ROOM"
```

draft 文件落在 `$HIVE_STATE/volumes/<draft-volume>/<section>.md`，编辑器直接打开改。

## 设计要点（为什么是这样切的）

### 为什么 4 个 Agent 而不是 1 个

- **专业化**：每个 Agent 只干一类活，prompt 简单、可单独迭代
- **Rank 差异化**：writer 需要 manager Rank（高 token 预算），reviewer 用 staff（保守）；单 Agent 做不到这种粒度
- **可观察**：`hive team` 显示每个 Agent 的剩余 quota 和状态，单 Agent 是黑盒
- **Volume 策略落地**：reviewer 的 `:ro` 挂载只在 Agent 边界有意义 —— 这是"沙箱拒写"而不是"prompt 里求别写"

### 为什么 corpus 在 Volume，不在 prompt 里

1. **跨 session 持久**：投 ICML 的 corpus 跟投 OSDI 的可以共享过往论文 —— `paper-corpus` 一个 volume 全用
2. **增量更新**：写完一篇就把它丢进 corpus，下一篇 writer 自动学到新风格；不用每次粘贴
3. **结构化访问**：writer 用 `fs_read` 读特定 section 样本（确定性 + 不进 LLM context 直到 ReAct 选用），不依赖 LLM "记得我的风格"

### Mock LLM 时的退化行为

没设 `OPENAI_API_KEY` 时 llmproxy 走 MockProvider —— 输入末尾的 user message 加 `"mock-summary:"` 前缀回声。

| Agent | mock 模式行为 |
|---|---|
| `paper-scout` (workflow) | 固定 3 步流水线 → **会**写 related.md，但内容是 mock 回声 |
| `paper-outline` (skill) | mock 不返回 JSON 工具调用 → 走 "plain answer" fallback → **不**读 corpus、**不**写 outline.md |
| `paper-writer` (skill) | 同上 |
| `paper-reviewer` (skill) | 同上 |

要看真实多步 ReAct（writer 真去 fs_read 三个文件后 fs_write）和真稿，必须设 LLM provider。

## 排错

- **`volume not found`** → `hive volume create <name>` 先建
- **`port 8992 already in use`** → 旧 demo 的 stub 没退，`pkill -f 'http.server 8992'`
- **skill agent 输出 `mock-summary: ...`** → 没设 API key，预期行为
- **draft 文件没产出** → 同上 + skill agent 在 mock 模式下不会 fs_write
- **reviewer 报 `permission denied: fs_write`** → **预期** —— reviewer 的 draft 是 ro 挂载（这是设计意图）
- **`hive run` 卡住** → 检查 `tail -f $HIVE_STATE/daemon.log`，多半是某个 agent 进程没起来

## 扩展 / 改造

- **加更多过往论文到 corpus**：`cp <my-paper>.md $HIVE_STATE/volumes/paper-corpus/` —— writer 立刻能 fs_read 到
- **改 writer 的章节规则**：编辑 `examples/paper-assistant/writer/SKILL.md`，重新 `hive build`
- **新增第 5 个 Agent（比如 `paper-figmaker`）**：复制 writer 目录，改 SKILL.md + agent.yaml 的 name，重新 build，加进 Hivefile 的 `agents:` 列表
- **接真 arxiv API**：把 scout 的 flow.json `net_fetch` 的 URL 换成真 arxiv 查询接口（注意 rate limit + 把 quota.api_calls.http 调高），或者写个 wrapper agent
