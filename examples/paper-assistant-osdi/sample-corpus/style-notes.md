# 写作风格 / 个人偏好（OSDI 投稿版）

> 这份文件覆盖通用 ML 版（`examples/paper-assistant/sample-corpus/`）的 OSDI-specific 部分。
> 共有的反模式（30 词以上长句、空话开头、客套话）继续有效。

## OSDI 写作顺序（重要差异 vs ML conf）

1. **Eval freeze**（数据/图表/baseline 比较定型，整篇围绕这些数字写）
2. System overview / 架构图
3. Design（机制 — mechanism-first）
4. Implementation（≤2 页，绝大多数细节进 appendix）
5. Eval 正文展开
6. Related Work（按 incumbent system 组织，不按时间）
7. Introduction（最后写：宏观→具体→contribution ≤4 条）
8. Abstract / Title

> 原则：Eval 是 OSDI 论文唯一硬通货 —— 永远先把数据 / 图表 / baseline 比较定下来，再写叙事。
> Implementation 章节常被压到 1-2 页，绝不要先写。

## OSDI Anti-patterns（务必避免）

### 修辞型
- novel / first time / groundbreaking / paradigm-changing / we are the first

### 大话型
- will / always / never / impossible / completely solves / silver bullet

### Eval 型（OSDI reviewer 杀手锏，pet peeve top list）
- 「只跟 ablation 比，不跟 incumbent system 比」 —— pet peeve #1
- 「微基准独大，没 end-to-end real workload」
- 「单机 / 单 NUMA / 单租户」 —— 分布式 / 多核 / 多租户 / 故障场景跑过吗？
- 「scope 过宽」 —— 一篇塞 3 个机制 → reviewer 觉得 pick one 深做
- 「artifact 缺失」 —— OSDI 现在 AE 强制评估，没 reproducible code 直接降分

## OSDI 必备 Baseline（按主题）

提交前 self-check：本文 contribution 对应 baseline 都比过了吗？

| 主题 | 必比 baseline |
|---|---|
| 存储 / KV | RocksDB / LevelDB / Anna / Bw-Tree / FASTER |
| 调度 | Linux CFS / Shinjuku / Shenango / Caladan / ghOSt |
| FS | ext4 / XFS / FUSE / NOVA / Strata |
| 网络 | DPDK / mTCP / eBPF / io_uring / Snap |
| 共识 | Raft / Multi-Paxos / EPaxos / Zab |
| 内核 | Linux baseline / unikernel / IX / Arrakis |

漏掉的 baseline 必须在 §Related 用一段说明为什么不直接对比（closed-source / 不同假设 / 不可比 measurement）。

## Section 写作规则（OSDI 版）

### Design
- Mechanism-first：先讲 data structure + 关键操作，motivation 1 段足够
- 配合一张架构图：组件框 + 数据流 + 控制流箭头
- 锁、内存 layout、复杂度分析在正文（不进 appendix）

### Implementation
- 总行数、语言、关键依赖（一段够）
- design 章节里没说清的工程细节（fsync 频率、batch 大小、threading model）
- ≤2 页

### Eval
- **第一张图**：summary table（mechanism × workload × metric × baseline），一页看清全部 claim
- 每个 claim 双路验证：microbenchmark + macro workload
- 必给：throughput-latency 曲线、p99/tail latency、CPU/memory overhead breakdown
- 失败模式：crash recovery / partial failure / network partition 至少一组
- 与 baseline 对比时同硬件、同 workload，参数公平（reviewer 会 grep "fair"）

### Related Work
- 按 incumbent system 组织（每个 baseline 一段），不按时间
- 每段先承认对方贡献，再说本文 differentiate 在哪
- ≤1.5 页

### Introduction
- 宏观背景（为什么这个 domain 重要）→ 现状不足（具体 gap，引 baseline）→ 我们的角度（一句话） → contribution（≤4 条 bullet）
- 最后写

### Abstract
- ≤200 词，4 段：背景 / 问题 / 我们做了什么 / 数字结果

## 时间表（提前 6 周计算）

- Submission - 6w / -5w：实验矩阵跑齐；artifact script 版本化
- Submission - 4w：Eval freeze + Design 写作
- Submission - 3w / -2w：Implementation + Eval 正文 + Related
- Submission - 1w：Intro + Abstract + AE appendix
- 「3-5 天 deep work block」专门写 §Eval —— 最难、最重要、最容易被打回的章节
