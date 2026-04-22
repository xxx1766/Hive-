# Hive 架构设计文档

> 🐝 **Hive = 多 Agent 的人才市场**  
> 让每个人才（Agent）能独立打包分发，让你轻松招聘（`hive hire`），组合完成复杂任务。

---

## 核心设计哲学

### 问题背景

现在 AI Agent 领域遇到了类似容器化之前的问题：

- ❌ 分发困难：Agent 能力没法像 Docker 镜像一样分享复用
- ❌ 依赖冲突：多个 Agent 放在一起容易依赖打架
- ❌ 安全不可控：随便一个第三方 Agent 就能乱删文件乱烧钱
- ❌ 组合麻烦：要复用别人的 Agent 得改代码重新打包

Hive 借鉴了操作系统的虚拟化思想，把**虚拟化层级从操作系统提升到了 Agent 能力层面**。

---

## 核心术语表（招聘/职场类比）

| Hive 概念 | 命名 | 含义 | 对应 Docker/OS 概念 |
|-----------|------|------|-------------------|
| 整个系统 | 🐝 **Hive** | 人才市场（蜂群），一语三关：`Hire` + `Hive` + `Give me five 🖐️` | - |
| 隔离命名空间 | 📍 **Room** | 一个项目一间会议室，一组 Agent 在一个 Room 里干活，隔离不干扰 | Namespace |
| 权限配额控制 | 👔 **Rank** | 职级权限，不同职级能使用不同资源、访问不同东西 | Cgroups + 访问控制 |
| Agent 清单 | 📝 **`agent.yaml`** | 单个 Agent 的 manifest（身份、entry、默认 Rank、默认配额） | Dockerfile |
| Room 描述 | 📝 **Hivefile** (`*.yaml`) | 一组 Agent 的编排方案 —— 声明 Room 里招哪些人、Rank 怎么改 | docker-compose.yaml |
| 打包镜像 | 📦 **Hive Image** | 打包好的人才包，一个技能/一个 Agent | Docker Image |
| 公共 Registry | 📢 **Hive Registry** | 人才市场，发布和发现 Agent。MVP 阶段直接把 GitHub 公开目录当 registry 用（见 `registry/README.md`），独立 Hub 服务留到 v2 | Docker Hub |
| 拉取命令 | 🤝 **`hive hire`** | 招聘这个人才到你的 Room | `docker pull` |
| 运行命令 | 🚀 **`hive run`** | 开工，启动项目 | `docker run` |
| 构建命令 | 🔨 **`hive build`** | 按 `agent.yaml` 打包一个 Agent Image | `docker build` |
| 编排命令 | 🪄 **`hive up`** | 按 Hivefile 拉起一个 Room + 批量招聘里面所有 Agent | `docker compose up` |

---

## 架构分层对比

### 传统方式：在 Docker 上跑 Agent

```
┌─────────────────────────────────────────────────────────┐
│                 用户业务逻辑 / 主 AI                        │
├─────────────────────────────────────────────────────────┤
│            所有 Agent 都硬编码在这里                        │
│           (所有 Agent 共享同一个进程空间)                   │
├─────────────────────────────────────────────────────────┤
│                   Docker 容器（整个 OS）                     │
│  • 对 Agent 语义完全无知                                   │
│  • 隔离粒度是整个容器，很重                               │
│  • 安全控制很粗：容器里想干嘛干嘛                         │
├─────────────────────────────────────────────────────────┤
│                     宿主机内核                               │
└─────────────────────────────────────────────────────────┘
```

**特点：**
- 虚拟化在操作系统层级
- 打包分发单位是"整个应用"
- 单个 Agent 很难复用
- 依赖冲突常见，安全控制粗

---

### Hive 方式：Agent 能力级虚拟化

```
┌─────────────────────────────────────────────────────────────┐
│  🐝 Hive 人才市场系统                                          │
│                                                              │
│  📍 Room 1：论文写作项目（隔离命名空间）                       │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  👔 Rank=Intern → arXiv 搜索 Agent                  │    │
│  │     • Token 配额：1000 tokens                       │    │
│  │     • 允许访问：api.arxiv.org                       │    │
│  │     • 禁止执行命令、禁止写文件                      │    │
│  ├─────────────────────────────────────────────────────┤    │
│  │  👔 Rank=Staff → 写作 Agent                         │    │
│  │     • Token 配额：10000 tokens (openai/gpt-4o)     │    │
│  │     • 允许读写：项目目录                            │    │
│  │     • 允许执行：python, git                         │    │
│  ├─────────────────────────────────────────────────────┤    │
│  │  👔 Rank=Manager → 审核发布 Agent                   │    │
│  │     • 允许推送 Git 到远程仓库                       │    │
│  │     • 较高配额                                     │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                              │
│  📍 Room 2：数据分析项目（另一个隔离空间，互不干扰）          │
│  ┌─────────────────────────────────────────────────────┐    │
│  │        不同 Room 权限隔离，数据不通，资源配额分开算    │    │
│  └─────────────────────────────────────────────────────┘    │
├─────────────────────────────────────────────────────────────┤
│ 🔐 Hive Runtime：协调 + Rank 权限检查                         │
│  • 流程编排                • 依赖解析                        │
│  • 配额统计                • 访问控制                        │
├─────────────────────────────────────────────────────────────┤
│              共享宿主机软件层（Python/CLI/OS）                │
│  Hive 不虚拟化整个操作系统，只做能力级访问控制                 │
└─────────────────────────────────────────────────────────────┘
```

**特点：**
- 虚拟化在 **Agent 能力层级**，在操作系统软件之上
- 打包分发单位是**单个专业 Agent**，可独立复用
- Room 做命名空间隔离，不同项目不同 Room，互不干扰
- Rank 做权限配额控制，语义级精准控制，比 Docker 更安全

---

## Rank 职级权限设计

Rank 对应操作系统的 Cgroups + 访问控制，但基于 AI Agent 场景做了分类：

### 默认职级模板

| 职级 | 图标 | 权限范围 | 适合场景 |
|------|------|---------|---------|
| 👶 **intern** | 实习生 | 仅 API 调用，只读指定文件，无命令执行，低配额 | 搜索、数据整理类 Agent |
| 👔 **staff** | 正式员工 | 读写项目目录，执行常用开发工具，中等配额 | 写作、编码类 Agent |
| 👨‍💼 **manager** | 经理 | 访问 Git、网络、系统工具，较高配额 | 项目协调、部署类 Agent |
| 👑 **director** | 总监 | 全权限，可安装软件，高配额 | 系统管理 Agent |

### 可配置权限分类

用户可以基于默认模板自定义，支持六大类控制：

| 分类 | 控制内容 | 示例 |
|------|---------|------|
| **🔤 Token 配额** | 大模型调用 token 计数限制 | `openai:gpt-4o` 最多 10000 tokens |
| **🔑 API 配额** | 第三方 API 调用次数限制 | `google:search` 最多 50 次/运行 |
| **💾 文件系统** | 读写权限白/黑名单 | 允许读 `./data`，禁止读 `~/.ssh`，禁止写 `/` |
| **🖥️ 命令执行** | CLI 工具白/黑名单 | 允许 `git` `python`，禁止 `rm` `curl` |
| **🌐 网络访问** | 域名/IP 访问控制 | 只允许 `api.openai.com`，禁止访问内网 |
| **🔁 调用控制** | 嵌套调用深度/次数 | 防止无限递归调用 |

### 共享连接 vs 隔离配额

一个常见问题：**不同 Room 里的同一个 Agent，实际上调用的还是同一个底层 API 吗？**

是的，Hive 做了连接共享 + 配额隔离的分层设计：

```
┌─────────────────────────────────────────────────────────────┐
│  📍 Room A：项目1                                            │
│  ├─ 👔 Agent 1 → 配额：10k token  ← 🔢 独立配额              │
│  └─ 👔 Agent 2 → 配额：5k token   ← 独立计数                │
└────────────────┼────────────────────────────────────────────┘
┌────────────────┼─────────────────────────────────────────────┐
│  📍 Room B：项目2                                            │
│  ├─ 👔 Agent 1 → 配额：10k token  ← 🔢 独立配额（不受A影响） │
│  └─ 👔 Agent 3 → 配额：20k token                            │
└────────────────┼────────────────────────────────────────────┘
         ▼
    ┌──────────────────┐
    │  OpenAI API 连接池 │  ← 🔌 同一个 API Key 复用连接
    └──────────────────┘
```

| 层面 | 设计 |
|------|------|
| **TCP 连接 / API 客户端** | ✅ **共享复用**：同一个 API Key 只维护一个连接池，节省握手开销 |
| **配额计数 / 权限控制** | ❌ **独立隔离**：每个 Agent 在每个 Room 里单独计数，一个超配额不影响其他 |

如果你配置了多个 API Key，Hive 会自动维护多个连接池，完全分开互不干扰。

---

## 与现有方案的本质区别

### vs Claude Code / VS Code Plugins / MCP

| 维度 | Claude Code Plugins | Hive |
|------|-------------------|------|
| **架构** | 一个大主 AI + N 个工具插件 | N 个专业 Agent + Hive 协调层 |
| **决策** | 中心大 AI 做所有决策，插件只是工具 | 每个 Agent 在自己领域做决策 |
| **分发** | 插件（工具） | 完整可独立运行的 Agent |
| **开放性** | 必须在宿主平台内使用 | 不绑定任何平台，随处可运行 |
| **组合** | 平台官方支持才能组合 | 任何人可任意组合、嵌套调用 |

**一句话区别：**
- Claude Code：**一个聪明的老板带一群打工工具**
- Hive：**一群专业专家分工协作，没有臃肿的老板**

---

### vs Docker / 容器

| 维度 | Docker | Hive |
|------|--------|------|
| **虚拟化层级** | 操作系统级 | Agent 能力级 |
| **隔离粒度** | 容器（整个应用） | Room（一组 Agent） + Rank（每个 Agent） |
| **打包单位** | 整个应用 | 单个 Agent 能力 |
| **资源占用** | 每个容器一个完整 OS，较重 | 共享宿主机环境，极轻量 |
| **安全控制** | 粗粒度进程隔离 | 语义级精准权限配额 |
| **启动速度** | 秒级 | 毫秒级 |

---

## 架构扩展方向

MVP 跑通之后，有两个面向未来的架构决策已经敲定（落成 ADR 给后续实现），但实现本身还在 TODO 里。这里记在架构文档是因为这两件事影响产品定位，不是普通特性。

### 与外部 AI 工具（Claude Code / Cursor / MCP / LLM）的关系

**依赖方向**：Hive 始终在上，外部 AI 工具当**算力后端**。用户入口永远是 `hive` CLI；Agent 通过 Hive 的 proxy 层调外部 AI 工具。**反方向**（把 Hive 作为 Claude Code 的插件 / MCP server 让宿主 AI 工具来调）**不是第一阶段目标**。

```
┌──────────────────────── hived ────────────────────────┐
│  User ─▶ hive CLI ─▶ dispatcher ─▶ Room ─▶ Agent       │
│                                              │         │
│                                              ▼         │
│   ┌──────────────────────────────────────────────┐     │
│   │  llmproxy   mcpproxy   aitoolproxy  netproxy │     │
│   │  (直调 LLM) (MCP srv)  (CLI 工具)    (HTTP)   │     │
│   └──────────────────────────────────────────────┘     │
│        │           │              │            │      │
└────────┼───────────┼──────────────┼────────────┼──────┘
         ▼           ▼              ▼            ▼
   OpenAI /     MCP server    Claude Code /   任意外部
   Anthropic    (stdio/HTTP)  Cursor CLI      SaaS API
```

**四类后端的统一处理**

| 后端 | proxy | 传输 | Rank 控制位 | 配额 key |
|---|---|---|---|---|
| 直调 LLM（OpenAI / Anthropic / Groq …） | `llmproxy` | HTTP | `LLMAllowed`（已实现） | `tokens:<model>` |
| MCP server | `mcpproxy`（待建） | stdio / HTTP | `MCPAllowed`（待加） | `api_calls:mcp:<server>` |
| CLI 类 AI 工具（Claude Code / Cursor） | `aitoolproxy`（待建） | exec 子进程 | `AIToolAllowed`（待加） | `api_calls:ai_tool:<name>` |
| 普通 SaaS API（兜底） | `netproxy` | HTTP | `NetAllowed`（已实现） | `api_calls:http` |

**不变量**（与前文"共享连接 vs 隔离配额"同条）：同一后端的连接 / 会话在 proxy 内进程级复用；配额按 `(Room, Agent, 资源)` 三元组独立计数。新增的 proxy 也必须落在这两条轨道里。

### Agent 打包形态（`manifest.kind` 字段）

MVP 只支持"Go 编译二进制 + stdio JSON-RPC"。扩展为四种形态，用 manifest 的 `kind` 字段显式区分：

| kind | 是什么 | hived 怎么跑它 | 阶段 |
|---|---|---|---|
| `binary` | 用户自己编的可执行文件（任意语言） | 直接 exec manifest 里的 `entry` | ✅ 已实现（省略时即此默认） |
| `skill` | 一份 `SKILL.md` + 工具声明 | `hive-skill-runner` 作为 entry，读 md → 驱动 LLM 循环 | ✅ 已实现 |
| `workflow` | 静态 `flow.json` **或** LLM 规划 `PLANNER.md`（二选一） | `hive-workflow-runner` 作为 entry；静态模式直接执行；LLM 模式让 LLM 产生 workflow 再执行 | ✅ 已实现 |
| `script` | Python / Node / Bash 脚本 | 沙箱 bind-mount 对应解释器，exec 脚本 | 🚀 v2 |

**Manifest 示例**：

```yaml
# 现有（省略 kind 即为 binary）
kind: binary
entry: bin/fetch

# 新增：skill
kind: skill
skill: SKILL.md
model: gpt-4o-mini
tools: [net, fs, peer]       # 声明允许使用的 Hive 代理

# 新增：workflow — 静态
kind: workflow
workflow: flow.json
tools: [net, llm]

# 新增：workflow — LLM 规划
kind: workflow
planner: PLANNER.md
model: gpt-4o-mini
tools: [net, llm]

# v2：script
kind: script
runtime: python@3.11
entry: main.py
deps: requirements.txt
```

**skill-runner 子系统**

`cmd/hive-skill-runner/main.go` 是 Hive 编出的第三个二进制（与 `hive` / `hived` 并列）—— 作为 `kind: skill` Agent 的实际执行体：

- daemon 检测到 `kind: skill` 时，把 `hive-skill-runner` 当成 entry 传给 `ns.NewAgentCommand`
- runner 在沙箱内跑，通过 stdio JSON-RPC 连回 daemon —— **和普通 Agent 一模一样**
- 内部循环：SKILL.md 作为 system prompt → `llm/complete` → 解析 LLM 返回的工具调用 → 转发到 `net/fetch` / `fs/read` / `peer/send` 等代理 → 结果回填给 LLM → 直到 LLM 决定 `task/done`

**关键设计点**：runner 虽然 Hive 自带，但对 hived 来说它仍然只是一个普通 Agent 子进程 —— 同样的 namespace 沙箱、同样的 Rank、同样的配额扣减。sandbox 隔离不因内置而弱化；第三方也可以写自己的 runner 替代。

**向后兼容**：`kind` 省略时默认 `binary`，现有 Image 无需改动。

---

## 完整工作流示例

### 场景：我要写一篇关于 LLM 蒸馏的调研论文

```bash
# 1. 创建新项目，开一个 Room
hive init llm-paper

# 2. 招聘需要的专家
hive hire hive/arxiv-search     # 招聘一个 arXiv 搜索专家
hive hire hive/paper-summarize  # 招聘一个论文总结专家
hive hire hive/writer           # 招聘一个写作专家
hive hire hive/latex-builder   # 招聘一个 LaTeX 编译专家

# 3. 查看团队职级
hive team

# 输出：
# 📍 Room: llm-paper
#  👔 intern  hive/arxiv-search
#  👔 intern  hive/paper-summarize
#  👔 staff   hive/writer
#  👔 manager hive/latex-builder

# 4. 开工！
hive run "帮我写一篇关于 LLM 知识蒸馏最新进展的调研论文"
```

**Hive 内部做了什么：**
1. `arxiv-search` (intern) → 搜索 arXiv，找最新相关论文（配额 1k token，只能上网不能写文件）
2. `paper-summarize` (intern) → 总结每篇论文要点（只读搜到的结果，配额 5k token）
3. `writer` (staff) → 根据总结写出完整论文（能写项目目录，配额 20k token）
4. `latex-builder` (manager) → 编译生成 PDF（能执行 pdflatex，能输出最终文件）

整个过程每个 Agent 只在自己权限内工作，配额用完自动停止，安全又省心。

---

## 核心优势总结

✅ **可复用**：单个 Agent 独立打包分发，一次编写到处 hire  
✅ **安全**：Rank 职级精准控制，第三方 Agent 也敢用，不会乱烧钱乱删文件  
✅ **轻量**：不需要每个 Agent 一个容器，共享环境，毫秒启动  
✅ **灵活**：支持任意嵌套，一个 Hive 可以 hire 别的 Hive，就像函数调用  
✅ **开放**：不绑定任何模型/平台，OpenAI、Anthropic、本地模型都能用  

---

## 一句话总结

> **Docker 把操作系统虚拟化，让微服务可以分发复用**  
> **Hive 把 Agent 能力虚拟化，让多 Agent 可以分工复用**

🐝 Happy Hiving!
