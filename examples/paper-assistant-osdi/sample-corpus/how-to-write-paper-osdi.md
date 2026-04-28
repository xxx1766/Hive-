# 论文写作方法论（OSDI 版精简）

## 关键差异 vs ML conf paper

OSDI 评审的硬通货是 **Evaluation**。所有写作动作围绕 eval 数据展开 —— 不是反过来。
ML conf 容许"漂亮 idea + ok eval"过线；OSDI 不会。

## 时间表（提前 6 周计算）

### -6w / -5w：Eval 设计 + 跑实验
- 实验矩阵：(workload × baseline × parameter) 全表先列出来
- 每条 claim 至少两组验证（microbenchmark + macro workload）
- artifact reproducibility：跑实验的脚本从一开始就版本化（git tag 起步）

### -4w：Eval freeze + Design 写作
- 数据 / 图表 / baseline 比较定型，不再加新实验
- Design 章节起草，先画架构图

### -3w / -2w：Implementation + Eval 正文 + Related
- Implementation 控制在 1.5 页
- Eval 正文：summary table 第一张，每个 claim 一段叙事
- Related：按 incumbent system 组织

### -1w：Intro + Abstract + Title + AE appendix
- Intro 最后写：宏观 → 现状不足 → 我们的角度 → contribution（≤4 条）
- Abstract ≤200 词，4 段：背景 / 问题 / 我们做了什么 / 数字
- Artifact appendix + Docker/script 整理好

## 反模式（OSDI 特有）

- **修辞**：novel / first time / groundbreaking / paradigm-changing
- **eval 单薄**：只 microbenchmark，没 real workload
- **baseline 缺**：incumbent system 不比 → reviewer 直接 reject
- **scope 过宽**：一篇 3 个机制 → 反馈 "pick one 深做"
- **单机**：分布式 / 多 NUMA / 多租户 / 故障场景没跑

## 启发式

- 每图 3-4 数据组、单页一图、灰度可读、caption self-explanatory
- 一定要 deep-work block 写 §Eval —— 最难、最重要、最容易被打回的章节
- 提交前 self-check 跟着 `osdi-reviewer-checklist.md` 跑一遍
- 「一定要动手跑一跑代码」 —— 永远不要只在脑里推 design
- Implementation 细节先写在 lab notebook，再压成正文 1.5 页
