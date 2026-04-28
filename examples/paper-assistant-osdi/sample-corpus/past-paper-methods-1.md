# 作者过往 Systems 论文 §Design 节选（语调样本，OSDI 风格）

> 用途：写 §Design / §Implementation 时 mimic 这种语调 —— mechanism-first，
> 具体 data structure，所有数字 claim 都带 §-references 指向 §Eval。

## 3. Design

Klesh is a tiered KV-store with three layers: an in-memory hash index (L0), a per-core append-only log (L1), and a shared LSM on NVMe (L2). The contribution lies not in the layering itself — many systems do this [12, 23] — but in *who decides when a key migrates between tiers*. Existing tiered stores rely on static thresholds (LRU age, frequency cutoff). Klesh delegates the migration decision to a per-CPU cost model that the system updates online from observed access patterns.

### 3.1 Hot/cold classification

Each L0 entry carries an 8-bit recency counter and a 4-bit access histogram over a 1024-access exponentially-decayed window. A per-core migration thread runs every 100ms and computes:

    cost(key) = α · access_rate(key) − β · L1_amortized_io_per_demote

α and β are tuned online during a 30s warmup phase (§3.4). Keys with cost < 0 are demoted to L1 in batches of up to 256 to amortize fsync.

### 3.2 Crash consistency

L1 fsyncs in 4MB chunks; an in-memory chunk-map records the LSN of the latest durable chunk. On recovery, Klesh replays L1 from the last checkpoint LSN and rebuilds L0's hash index from L1+L2 in O(N_keys). We measure 380ms recovery for 10M keys on NVMe (§5.4) — 5.5× faster than RocksDB on the same workload.

### 3.3 Concurrency

L0: per-bucket spinlocks, 32-byte aligned to avoid false sharing.
L1: per-core, no contention by construction.
L2: single-writer per shard, wait-free reader path via epoch-based reclamation.

The only globally-acquired lock is the checkpoint-LSN advance, held <100µs measured (§5.6).

---

**Stylistic notes** (for the writer Agent):

- **Mechanism-first**: data structure + ops in the first 2 sentences of each subsection; motivation comes second.
- **Baselines named**: every comparison cites a specific incumbent system in [n] form.
- **Numbers cite §Eval**: any quantitative claim has a §-reference to where the data lives.
- **Complexity in-line**: not deferred to appendix.
- **No marketing**: zero occurrences of "novel", "first", "groundbreaking", "paradigm".
- **Sentence length**: every sentence here is under 30 words.
