# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 仓库状态

本仓库处于**尚未实现**阶段。目前只有一份设计文档（`ARCHITECTURE.md`，中文撰写）、一份几乎为空的 `README.md`，以及一份 `claude-flow.config.json`。没有任何源代码、构建系统、测试套件、lint 配置或 CLI 可执行文件。请不要凭空编造 build/test 命令 —— 如果用户要求构建或跑测试，先确认希望用哪种语言/工具链来搭建骨架。

主设计文档：`ARCHITECTURE.md`。在提出任何结构性设计之前务必先读它 —— 它是术语和项目范围的唯一权威来源。

用户用中文交流；当用户用中文提问时用中文回答，但代码标识符保持英文。

## 产品愿景

Hive 是一套**面向多 Agent AI 的能力级虚拟化系统** —— 也就是 Docker for Agents。`ARCHITECTURE.md` 的核心洞察是：今天的 Agent 面临的分发、依赖、安全问题，和当年应用面临的是同一类问题，但容器化把虚拟化做在了错误的层级。Docker 虚拟化操作系统；Hive 虚拟化 *Agent 的能力*，让单个专业 Agent 可以独立打包、共享、组合、配额管控，而无需给每个 Agent 起一个完整容器。

`ARCHITECTURE.md` 中明确列出的定位对比：
- **vs. Claude Code / MCP 插件**：插件是一个主 AI 底下的工具；Hive 里的 Agent 是各自拥有决策权的独立专家，不绑定任何宿主平台。
- **vs. Docker**：隔离发生在 Agent 能力层，而不是操作系统层 —— 不需要为每个 Agent 起容器，毫秒级启动，语义级权限控制代替粗粒度的进程隔离。

## 核心术语（不要改名）

招聘/职场隐喻是有意为之，并且面向用户。在代码、CLI 和文档中都要保留这套命名。

| 概念 | 命名 | 作用 | Docker 对应 |
|---|---|---|---|
| 系统整体 | **Hive** | 人才市场（一语三关：Hire + Hive + high-five） | — |
| 隔离命名空间 | **Room** | 一个项目就是一间会议室；同一个 Room 内的 Agent 共享上下文，不同 Room 之间互相隔离 | Namespace |
| 权限/配额职级 | **Rank** | 职级：`intern` / `staff` / `manager` / `director`；决定每个 Agent 能做什么 | cgroups + ACL |
| 声明文件 | **Hivefile** | 声明要招聘哪些 Agent 以及它们如何协作 | Dockerfile |
| 打包好的 Agent | **Hive Image** | 可分发的单一能力 Agent | Docker Image |
| 公共仓库 | **Hive Registry** | 发布和发现 Agent 的地方 | Docker Hub |
| 拉取 | `hive hire` | 把一个 Agent 招进当前 Room | `docker pull` |
| 运行 | `hive run` | 启动项目 | `docker run` |
| 构建 | `hive build` | 从 Hivefile 构建镜像 | `docker build` |
| 初始化 | `hive init <project>` | 创建新项目 / 新 Room | — |
| 查看团队 | `hive team` | 列出当前 Room 里已招到的 Agent 以及各自的 Rank | — |

## Rank 的设计（安全模型）

Rank 是安全和配额的最小原语。任何新加的权限或资源管控都应该落在下面这六类里（来自 `ARCHITECTURE.md`），不要另起一套平行模型：

1. **Token 配额** —— 单个 Agent 的大模型 token 预算（例如 `openai:gpt-4o` → 10k tokens）
2. **API 配额** —— 第三方 API 的调用次数（例如 `google:search` → 50 次/运行）
3. **文件系统** —— 读写白名单和黑名单（例如允许 `./data`，禁止 `~/.ssh`，禁止写 `/`）
4. **命令执行** —— CLI 白名单/黑名单（例如允许 `git python`，禁止 `rm curl`）
5. **网络访问** —— 域名/IP 级访问控制
6. **调用控制** —— 嵌套深度和递归上限

默认职级模板：`intern`（仅 API、只读、低配额）→ `staff`（项目目录读写、开发工具、中配额）→ `manager`（Git / 网络 / 系统工具、高配额）→ `director`（全权限、可安装软件、最高配额）。

## 关键架构不变量：连接共享、配额隔离

摘自 `ARCHITECTURE.md` §"共享连接 vs 隔离配额" —— 这是整份实现里最重要的一条约束：

- **TCP 连接 / API 客户端是共享的**：跨 Room 和 Agent、只要用同一个 API Key，就复用同一个连接池，省掉重复握手的开销。
- **配额计数和权限是隔离的**：以 `(Room, Agent)` 为粒度独立计数 —— 某个 Agent 触顶绝不能影响到其他 Agent 或其他 Room。

如果你去实现 runtime，这两件事必须落在不同层，不要合并到一处。

## 仓库目录结构

```
/data/Hive-
├── ARCHITECTURE.md           # 设计规约（中文）—— 术语与愿景的权威来源
├── README.md                 # 占位文件 —— 目前只有项目名
├── claude-flow.config.json   # claude-flow v3.5 协调配置（memory / swarm / hooks）
└── .claude-flow/             # 运行时状态（daemon.pid、logs）—— 思考源码时忽略
```

`claude-flow.config.json` 配的是**开发 Hive 时所用的** claude-flow 编排器，不是 Hive 自己的 runtime。不要把二者混为一谈：改它只影响 Claude Code / swarm 工具在本仓库里的行为，不影响 Hive 的架构。

## 扩展本项目时

- **语言/技术栈尚未选定。** 中文架构文档并未承诺某一种实现语言。在 Go / Rust / Python / TypeScript 之间下决定前务必先问用户 —— 这一选择会影响到连接池、配额强制、沙箱等 runtime 层的能力，后期很难反悔。
- **CLI 体验是项目身份的一部分。** `hive hire / run / build / team` 这组动词就是产品身份，即便底层实现换掉也要保留这套命名。
- **别把 Docker 套回来。** 整个项目的出发点就是 Hive **不是**操作系统级虚拟化。如果有人建议"干脆一个 Agent 一个容器就好了" —— 那恰恰违背了前提。沙箱应当落在能力层（进程级的 seccomp / 权限、语言层的权限包装、API 层的配额中间件），而不是容器层。
