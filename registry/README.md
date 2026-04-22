# Hive Registry (GitHub-hosted)

MVP 阶段把 GitHub 目录直接当 registry 用 —— 没搭独立 Registry 服务，蹭 GitHub 的 CDN / 版本管理 / 发现机制。

```
registry/
├── agents/          # 单个可分发 Agent（目前只收 kind=skill / kind=json —— 纯文本，无需编译）
│   └── brief/
│       ├── agent.yaml
│       └── SKILL.md
└── hivefiles/       # 成品 Room 编排方案
    └── skill-demo/
        └── Hivefile.yaml
```

## 怎么拉

三种 URL 写法都能识别：

```bash
# 1. github:// scheme（最明确）
hive hire my-room github://xxx1766/Hive-/registry/agents/brief

# 2. GitHub HTTPS tree/blob URL（直接从浏览器地址栏复制）
hive hire my-room https://github.com/xxx1766/Hive-/tree/main/registry/agents/brief

# 3. 短格式 owner/repo#path[@ref]（类 go-get）
hive hire my-room xxx1766/Hive-#registry/agents/brief@main
```

`@ref` 省略时默认 `main`；可以是 tag / branch / 完整 commit SHA。

拉完落在本地 `~/.hive/images/<name>/<version>/`，后续 `hive hire` 按 `name:version` 本地查找，离线也能用。

## Hivefile 也能远端拉

```bash
hive up github://xxx1766/Hive-/registry/hivefiles/skill-demo
```

Hivefile 里面 `agents:` 列表写远端 ref 时，daemon 递归 pull 每一个依赖 Agent。

## 安全须知

拉的是**别人仓库里的代码/提示词**，会在你本地 sandbox 里跑。Hive 的 Rank + namespace 做了一层兜底，但仍然建议：

- 只拉你信任的 owner/repo
- 用 `@<commit-sha>` 固定到特定 commit，避免 owner 事后改 main 分支偷换内容
- 自建 mirror / fork 是最稳的

## 贡献 Agent / Hivefile

本 MVP 阶段直接提 PR 到本仓库 `registry/` 即可。未来会分到独立 repo（见 README TODO §v2）。
