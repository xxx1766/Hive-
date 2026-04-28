# OSDI Reviewer Checklist（作者自审版）

> reviewer Agent 应该按这份 checklist 逐条审 draft，给出 ✓ / ⚠ / ✗ 评级 + 改写建议。
> 严格按照下面 6 个维度依次出结论；每条命中要带原文行号 / 句子。

## 1. 修辞反模式（grep 搜词）

- [ ] "novel" 出现次数（>0 即 ✗，列具体句子并给替代措辞）
- [ ] "first time" / "we are the first" / "groundbreaking" / "paradigm-changing"
- [ ] 绝对化表述：will / always / never / impossible / completely / silver bullet
- [ ] 客套开头："It is well known that ..." / "Recently, ..." / "Many works have ..."

## 2. Eval 完整性

- [ ] §Eval 第一张图是 summary table（mechanism × workload × metric × baseline）？
- [ ] 每个 claim 至少 2 组验证（microbenchmark + macro real workload）？
- [ ] throughput-latency 曲线 + p99/tail latency 都给了？
- [ ] CPU / memory overhead breakdown？
- [ ] crash / partial failure / network partition 至少一组？
- [ ] 与每个 baseline 同硬件、同 workload、参数公平（grep "fair"）？

## 3. Baseline 覆盖

- [ ] §Related 列出了至少 3 个 incumbent baseline？
- [ ] 每个 baseline 有解释为什么本文 differentiate？
- [ ] 漏掉的 obvious baseline 在 §Related 一段说明（closed-source / 不同假设 / 不可比 measurement）？

## 4. Scope discipline

- [ ] 本文一个机制深做，还是多个机制堆砌？（多个 → ⚠ 建议 pick one）
- [ ] Contribution 列表 ≤4 条？
- [ ] §Implementation ≤2 页？
- [ ] §Design 没有 5+ 段 motivation？（mechanism-first 原则）

## 5. 写作质量

- [ ] §Design 是 mechanism-first（数据结构 + 操作在前，motivation 在后）？
- [ ] 句长：随机挑 5 句，超过 30 词的拆短建议（带行号）
- [ ] §Intro 是宏观→具体→contribution 结构？
- [ ] §Abstract ≤200 词，4 段结构？
- [ ] 数字 claim 都有 §-reference 指向 §Eval 里的数据？

## 6. Artifact / 可复现性

- [ ] 代码 + script + 图脚本归档？
- [ ] artifact appendix（OSDI AE 必填）？
- [ ] 关键参数表 + workload 配置文件？

---

## 输出格式（reviewer Agent 必须用这个结构返回 answer）

```
## Review of <section>.md

### 1. 修辞反模式
- ✗ 行 5: 'we propose a novel mechanism' → 改: 'we present a mechanism that ...'
- ⚠ 行 28: 'It is well known that ...' → 删

### 2. Eval 完整性
- ⚠ 缺 summary table（建议放在 §5.1 第一张图）
- ✓ throughput-latency 曲线已有

### 3. Baseline 覆盖
- ✗ 没比 RocksDB（KV 类必比）
- ⚠ Anna 在 §Related 提到但没 §Eval 数据

### 4. Scope
- ✓ 一个机制深做

### 5. 写作质量
- ⚠ 行 42-44 一句 38 词，建议拆成两句

### 6. Artifact
- ✗ 没看见 artifact appendix

### Overall
3 个 ✗，4 个 ⚠ —— critical revisions before submission；优先级 1）补 RocksDB baseline，2）加 summary table，3）写 artifact appendix。
```

找不到问题就直接说 "no anti-patterns matched, no coverage gaps detected"，不要客套。
