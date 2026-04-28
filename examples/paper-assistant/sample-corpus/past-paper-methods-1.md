# 作者过往论文 Methods 节选（语调样本）

## 3. Method

We start with the observation that, on long-context inputs, attention's quadratic memory cost is dominated by tokens whose contribution to the final softmax is nearly zero. Concretely, in 80% of attention heads we measured, fewer than 1/8 of keys account for >95% of the softmax mass. This motivates a selection-then-attend design.

### 3.1 Selective Attention

Given query $q$ and keys $K \in \mathbb{R}^{n \times d}$, we compute a cheap relevance score $s_i = q^\top \tilde{k}_i$ where $\tilde{k}_i$ is a low-rank sketch of $k_i$. We retain the top-$m$ keys ($m = n/8$ in our experiments) and run standard softmax attention over them.

### 3.2 Why a learned sketch beats heuristics

Sliding-window and BigBird-style fixed sparsity patterns commit to a topology before seeing the input. Our sketch is task-trained jointly with the model, so it adapts to the actual head specialisation. We show in §5.3 that this matters most on heads that handle coreference.

(Stylistic notes for the writer Agent: no "novel" anywhere; intuition-first, then formula; complexity / implementation deferred to Appendix B; no sentence over 30 words.)
