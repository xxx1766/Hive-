# UI 教程：用 md↔doc 转换器走完整 HTTP UI 流程

这个 demo 用一个简单的「Markdown ↔ 正式文档样式」转换任务，串起 Hive HTTP UI 的每个能力——kanban / 时间线 / 跨 Room 创建 / Team 树 / Volume 浏览。读完你应该能：

1. 知道 UI 长啥样、每个 tab 对应什么
2. 会用「+ New conversation」弹窗创建普通会话和**跨 Room 会话**
3. 能从时间线读懂 round / from→to / payload
4. 知道哪些事情 UI 现在还做不了（要回 CLI）

## 这个 demo 在做什么

两个 skill agent，都跑在 GMI 的 `gpt-5.4-mini` 上：

| Agent | 输入 | 输出 |
|---|---|---|
| `md-to-doc` | 随手写的 markdown（短标题、bare bullets） | 正式文档样式（编号 heading、整句、引导段） |
| `doc-to-md` | 正式文档样式 | 还原回随手写的 markdown |

会有两种玩法：
- **单 Room**：一个 Room 同时 hire 两个 agent，从 UI 单独触发任一方向。
- **跨 Room**：两个 Room 各 hire 一个 agent，`md-to-doc` 通过 `peer_call` 把结果丢给 Room B 的 `doc-to-md` 跑 round-trip 验证。

## 准备

### 0. 设置 GMI 凭据

```sh
export OPENAI_API_KEY=<your-gmi-token>      # GMI 颁发的 JWT
export OPENAI_BASE_URL=https://api.gmi-serving.com/v1
```

`hived` 的子进程会继承这两个变量，agent 调 LLM 时直接用。

### 1. Build agents

```sh
cd /data/Hive-
./bin/hive build examples/md-doc-converter/md-to-doc
./bin/hive build examples/md-doc-converter/doc-to-md
```

应该看到：

```
built md-to-doc:0.1.0 at /root/.hive/images/md-to-doc/0.1.0
built doc-to-md:0.1.0 at /root/.hive/images/doc-to-md/0.1.0
```

### 2. 启动 daemon

```sh
./bin/hived &
```

daemon 默认监听 HTTP 在 `127.0.0.1:8910`，启动日志末尾会有 `httpapi: listening on http://127.0.0.1:8910`。

### 3. 浏览器打开 UI

```
http://127.0.0.1:8910
```

第一眼看到的布局：

```
┌──────────────────────────────────────────────────────────┐
│ Hive  [Room ▾ (no rooms)]                          live  │
├─[ Conversations ][ Volumes ][ Team ]─────────────────────┤
│                                                          │
│   no rooms hired — run `hive hire -f ...` first          │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

UI **只读**：它能创建会话、看现有 Room，但不能 hire/fire agent。所以需要回 CLI 来 hire。

---

## 单 Room 流程

### 4. Hire 一个 Room，同时塞两个 agent

```sh
./bin/hive hire -f hivefiles/md-doc-single.yaml
# 输出：
#   room md-doc-single-1778136273 created
#   hired md-to-doc:0.1.0
#   hired doc-to-md:0.1.0
```

刷新浏览器 → 顶部 Room 下拉里出现 `md-doc-single  (0 convs)`。

### 5. 点 Team tab 看团队

```
┌─[ Conversations ][ Volumes ][ Team ]─────────────────────┐
│ Team in md-doc-single                                    │
│                                                          │
│   • md-to-doc   [staff]   gpt-5.4-mini    quota: 16k     │
│   • doc-to-md   [staff]   gpt-5.4-mini    quota: 12k     │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

两个 staff 级 agent 平行存在。如果是 `paper-supervisor` 那种带 hire_junior 的 demo，这里会显示成 parent → child 缩进树。

### 6. 创建第一个会话

- 切回 **Conversations** tab
- 右上角点 **+ New conversation**

弹窗（默认值已经填好，下面打 ★ 是要改的字段）：

```
┌─ New conversation ───────────────────────────────────┐
│  Tag (optional)         [smoke-test            ]  ★  │
│  Target agent           [md-to-doc  [staff]  ▾ ]     │
│  Initial input (JSON)   ┌────────────────────────┐   │
│                         │{"markdown":"# Cache\n\n│★  │
│                         │\n- LRU eviction\n- 64MB│   │
│                         │ cap\n"}                │   │
│                         └────────────────────────┘   │
│  Max rounds             [4]                          │
│  ☑ Start immediately                                 │
│  ─────────────────────────────────────────────       │
│  ☐ Cross-Room — invite agents from other Rooms       │
│                                                      │
│                          [Cancel]  [ Create ]        │
└──────────────────────────────────────────────────────┘
```

填的 markdown 是任何随手写的笔记都行，只要是合法 JSON。点 **Create**。

### 7. 看 kanban

会话立刻出现在 **In progress** 列：

```
┌─ Planned ──┬─ In progress ─────────────┬─ Done ──┐
│            │ ┌─────────────────────┐   │         │
│  empty     │ │ active   smoke-test │   │  empty  │
│            │ │ → md-to-doc   0/4 rds│   │         │
│            │ │ ●●●  (typing dots)   │   │         │
│            │ │ 14:32:18             │   │         │
│            │ └─────────────────────┘   │         │
└────────────┴───────────────────────────┴─────────┘
```

`active` 状态会有打字泡泡动画，告诉你 daemon 在等 agent 回。等 5-15 秒（看 GMI 延迟），卡片会从 In progress 滑到 **Done** 列。

### 8. 看时间线

点会话卡片，右侧出现时间线面板：

```
┌─ smoke-test ───────────────────────[done]──[×]───────┐
│ 0/4 rounds                                           │
│                                                      │
│ 14:32:18  r0   (creator) → md-to-doc       [task_input]
│            { "markdown": "# Cache\n\n- LRU evict... }│
│                                                      │
│ 14:32:24  r0   md-to-doc                  [task_output]
│            { "answer": "{\"doc\":\"1. Cache\\n\\n   │
│              This section describes the cache       │
│              configuration.\\n\\n- LRU eviction is  │
│              used.\\n- The cap is 64MB.\"}" }       │
└──────────────────────────────────────────────────────┘
```

要点：
- **`r0`** = round 0 = task_input 和 task_output 都不算 peer 跳。Round 计数只计 peer 间的来回。
- **task_output** 的 `answer` 字段就是 agent 跑完 LLM 后吐出来的最终结果。这里可以看到原本一行的 `- LRU eviction` 被改写成了完整一句。
- 顶部状态徽章颜色：planned 灰、active 蓝、done 绿、failed 红、cancelled 橙、interrupted 黄。

要试反方向（doc → md），关掉时间线、再点 **+ New conversation**：
- Target agent 改成 `doc-to-md`
- Initial input 改成 `{"doc":"1. Cache\n\nThis section describes...\n\n- LRU eviction is used.\n..."}`
- Create

---

## 跨 Room 流程

### 9. Hire 两个 Room（每个一个 agent）

```sh
./bin/hive hire -f hivefiles/md-doc-room-a.yaml
./bin/hive hire -f hivefiles/md-doc-room-b.yaml
```

刷新浏览器，顶部下拉出现两条新 Room：

```
[Room ▾]
   md-doc-single        (1 convs)
   md-doc-room-a        (0 convs)         ← 选这个
   md-doc-room-b        (0 convs)
```

切到 `md-doc-room-a`。Team tab 里只看到 `md-to-doc` 一个 agent —— `doc-to-md` 在另一个 Room。

### 10. 创建跨 Room 会话

点 **+ New conversation**。这次注意弹窗底部那个 checkbox：

```
┌─ New conversation ───────────────────────────────────┐
│  Tag (optional)         [round-trip            ]     │
│  Target agent           [md-to-doc  [staff]  ▾ ]     │
│  Initial input (JSON)   ┌────────────────────────┐   │
│                         │{"markdown":"# Cache..","│   │
│                         │"verify":true}          │ ★ │
│                         └────────────────────────┘   │
│  Max rounds             [6]                          │
│  ☑ Start immediately                                 │
│  ─────────────────────────────────────────────       │
│  ☑ Cross-Room — invite agents from other Rooms    ★  │
│    ┌─────────────────────────────────────────────┐   │
│    │ [md-doc-room-b ▾] [doc-to-md ▾]   [+ Add]   │ ★ │
│    │                                             │   │
│    │ [md-doc-room-b / doc-to-md ×]               │   │
│    │                                             │   │
│    │ Initial target is auto-included; add agents │   │
│    │ from other Rooms here.                      │   │
│    └─────────────────────────────────────────────┘   │
│                                                      │
│                          [Cancel]  [ Create ]        │
└──────────────────────────────────────────────────────┘
```

操作步骤：
1. 在 input JSON 里加 `"verify": true` —— 这会触发 `md-to-doc` 跑 round-trip 模式（先 md→doc 自己做，然后 peer_call 给 `doc-to-md` 做反向）。
2. **勾上 Cross-Room** —— 下面那一坨控件展开。
3. Room 下拉里只会出现 `md-doc-room-a` **以外**的 Room（自己 Room 的 target 已经隐式在 members 里了）。选 `md-doc-room-b`。
4. Agent 下拉自动 fetch Room B 的 members，填上 `doc-to-md`。
5. 点 **+ Add** —— 出现一个紫色 chip：`md-doc-room-b / doc-to-md ×`。
6. **Create**。

### 11. kanban 上看到跨 Room 徽标

```
┌─ Planned ──┬─ In progress ────────────────────┬─ Done ──┐
│            │ ┌──────────────────────────────┐ │         │
│  empty     │ │ active  round-trip  ↔ 2 rooms│ │  empty  │
│            │ │ → md-to-doc          0/6 rds │ │         │
│            │ │ ●●●                          │ │         │
│            │ └──────────────────────────────┘ │         │
└────────────┴──────────────────────────────────┴─────────┘
```

紫色的 **`↔ 2 rooms`** 徽章就是跨 Room 标识，单 Room 会话不会出现。

### 12. 时间线带 Room 后缀

点开会话，时间线现在每条 from/to 都带 `· room-name` 后缀：

```
14:35:42  r0   (creator) → md-to-doc · md-doc-room-a   [task_input]
                { "markdown": "# Cache...", "verify": true }

14:35:49  r1   md-to-doc · md-doc-room-a → doc-to-md · md-doc-room-b   [peer]
                { "doc": "1. Cache\n\nThis section describes..." }

14:36:02  r2   doc-to-md · md-doc-room-b → md-to-doc · md-doc-room-a   [peer]
                { "answer": "{\"markdown\":\"Cache\\n\\n- LRU evic...\"}" }

14:36:08  r2   md-to-doc · md-doc-room-a                              [task_output]
                { "answer": "{\"doc\":\"1. Cache...\",\"roundtrip_md\":\"Cache..\"}" }
```

注意：
- m1 是初始输入（round 0）。
- m2 是 `md-to-doc` 主动 peer_call `doc-to-md`，**round 跳到 1**。
- m3 是 Room B 的 `doc-to-md` 回复，**round 跳到 2**。
- m4 是 `md-to-doc` 拿到回复后吐的 task_output（round 还是 2，task_output 不增 round）。
- 整条 transcript **只存在 owner Room（Room A）的目录下** —— 即使你切到 Room B，订阅的 SSE 也是 Room A 的 bus，看到的是同一份历史。

### 13. 验证 single-source transcript

```sh
# 切回 shell
ls /root/.hive/rooms/md-doc-room-a-*/conversations/
# 会有刚才那个 conv 的 JSON 文件

ls /root/.hive/rooms/md-doc-room-b-*/conversations/
# 是空的（或者只有 Room B 自己 owner 的 conv）
```

Room A 单一持久化是跨 Room 设计的核心不变量。

---

## 其他 tab

### Volumes

切到 Volumes tab，左侧列出所有命名 volume。本 demo 没用 volume —— 但如果你 hire 过 `paper-coordinator`，这里会有 `paper-osdi-corpus` 之类的，可以点进去浏览文件树。

### Team

每个 Room 的 Team tab 显示成员 + parent→child 树。本 demo 都是顶层 hire（无 parent），所以是平铺。如果有 agent 调用了 `hire_junior`，这里会显示成缩进树。

---

## 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| 弹窗 Create 时弹 alert：`Input is not valid JSON` | textarea 里写的不是合法 JSON | 检查引号、换行（用 `\n` 字面量） |
| 卡在 `active` 不动 | LLM 调用慢或挂了 | 看 daemon 日志：`tail -f /run/user/0/hive/agents/<member>/stderr.log`；超时会自动 cancel |
| 跨 Room create 报 `agent not hired in room` | Room B 没有 hire 你选的那个 agent | 回 CLI 跑对应的 hivefile |
| Cross-Room 勾上但 Room 下拉是空 | 现在只有一个 Room 存在 | 多 hire 几个 Room |
| `↔ N rooms` 徽章不显示 | summary 里 members 没传过来 | 确认是新版二进制（`./bin/hive version` 应当是 2026-05 之后的 commit） |

## UI 现在还做不了的

- **Hire / fire** —— 必须回 CLI（README §不支持的功能列表里有这条 TODO）。
- **远程访问** —— 默认绑 `127.0.0.1`，没有鉴权。要从别的机器看，得先做 token 或 mTLS。
- **Team tab 跨 Room 视图** —— 当前是「每个 Room 一棵树」。跨 Room 会话用到的成员要靠切 Room 看。

## 下一步

- 改 SKILL.md 试试更复杂的转换规则（保留代码块、处理表格、…）。
- 用 `volume` 让两个 agent 共享一个工作目录，把 input.md / output.doc.md 真的写到磁盘 —— UI 的 Volume 浏览器会显示出来。
- 看 `examples/cross-room-demo/` 里 chatter-a/b 的更小最小化跨 Room 例子（不调 LLM 也能跑）。
