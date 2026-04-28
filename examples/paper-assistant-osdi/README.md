# OSDI paper-assistant kit

Real-use companion to the generic `examples/paper-assistant/` demo, tailored to OSDI submission norms (eval-heavy, mechanism-first, baseline-disciplined).

**Reuses** the same 4 Agents — `paper-scout` / `paper-outline` / `paper-writer` / `paper-reviewer` — built from `examples/paper-assistant/`. Only the corpus + Hivefile differ.

## What's in this dir

```
sample-corpus/
├── style-notes.md                # OSDI anti-patterns + section rules + baseline-by-subdomain table
├── past-paper-methods-1.md       # synthetic OSDI-style §Design (tiered KV); writer mimics this voice
├── how-to-write-paper-osdi.md    # 6-week timeline; reverse-engineer eval-first
└── osdi-reviewer-checklist.md    # explicit ✓/⚠/✗ checklist the reviewer Agent applies
sample-arxiv/
└── papers.json                   # 3 systems-paper stubs (Caladan / Anna / io_uring) for scout
```

## Setup (one-time, after `make build`)

```bash
# 1. Build the 4 Agents (reused from the generic demo dir)
./bin/hive build ./examples/paper-assistant/scout
./bin/hive build ./examples/paper-assistant/outline
./bin/hive build ./examples/paper-assistant/writer
./bin/hive build ./examples/paper-assistant/reviewer

# 2. Create the OSDI-specific Volumes
./bin/hive volume create paper-osdi-corpus
./bin/hive volume create paper-osdi-draft

# 3. Seed the corpus volume
HIVE_STATE="${HIVE_STATE:-$HOME/.hive}"
cp examples/paper-assistant-osdi/sample-corpus/*.md "$HIVE_STATE/volumes/paper-osdi-corpus/"

# 4. (Optional but recommended) replace past-paper-methods-1.md with one of YOUR
#    past papers' §Design or §Methods — much better style mimicry. Then re-cp.
```

## Hire the team

```bash
ROOM=$(./bin/hive hire -f hivefiles/paper-assistant/osdi-paper.yaml)
echo "OSDI room: $ROOM"
```

## Use

```bash
# Lit survey (replace sample-arxiv/papers.json with real arxiv search results first;
# scout's flow.json hits http://127.0.0.1:8992/papers.json by default — adjust as needed)
./bin/hive run "$ROOM" --target paper-scout \
  '{"topic":"adaptive tiered storage for hot keys"}'

# Outline
./bin/hive run "$ROOM" --target paper-outline \
  '{"hypothesis":"per-CPU online cost model beats static-threshold tiering on mixed YCSB"}'

# Draft sections (OSDI conventions: design / implementation / eval / related / intro / abstract)
./bin/hive run "$ROOM" --target paper-writer '{"section":"design"}'
./bin/hive run "$ROOM" --target paper-writer '{"section":"eval"}'
./bin/hive run "$ROOM" --target paper-writer '{"section":"intro"}'
./bin/hive run "$ROOM" --target paper-writer '{"section":"abstract"}'

# Review each section as you finish it
./bin/hive run "$ROOM" --target paper-reviewer '{"section":"design"}'
./bin/hive run "$ROOM" --target paper-reviewer '{"section":"eval"}'
```

Drafts land at `$HIVE_STATE/volumes/paper-osdi-draft/<section>.md`. Open them in your editor.

## Tips

- **Set `OPENAI_API_KEY` + `OPENAI_BASE_URL`** — without these, llmproxy returns mock text and skill agents won't ReAct through tools. For non-OpenAI gateways (e.g. GMI):
  ```bash
  export OPENAI_API_KEY="$GMI_API_KEY"
  export OPENAI_BASE_URL="https://api.gmi-serving.com/v1"
  ```
- **Pick a non-default model at hire time** without editing yaml:
  ```bash
  ./bin/hive hire "$ROOM" paper-writer:0.1.0 \
      --model openai/gpt-5.4-mini \
      --quota '{"tokens":{"openai/gpt-5.4-mini":80000}}' \
      --no-prompt
  ```
  Or the interactive prompt asks for model + tokens together (model auto-fills the quota key).
- **Replace `sample-arxiv/papers.json`** with real arxiv exports (or wire scout to a live arxiv client) before running for real.
- **Add your past papers** to `paper-osdi-corpus/` as you accumulate — writer's style mimicry improves.
- **Reviewer is `:ro`** by Rank-as-policy — by design it cannot fix your draft, only annotate. Apply suggestions yourself.
- **Section names are OSDI-flavored** (design / implementation / eval). The writer's SKILL.md has ML-leaning examples but the corpus's `style-notes.md` overrides per-section guidance — the LLM weights the more specific source.

## Iterating on the agents themselves

If the writer keeps emitting ML-tinted prose despite the OSDI corpus, fork the agent locally:

```bash
cp -r examples/paper-assistant/writer examples/paper-assistant-osdi/writer
# edit examples/paper-assistant-osdi/writer/SKILL.md to bake in design/eval section rules
# bump version in agent.yaml so it doesn't collide with the generic build
./bin/hive build ./examples/paper-assistant-osdi/writer
# update hivefiles/paper-assistant/osdi-paper.yaml: paper-writer:0.1.0 → paper-writer-osdi:0.1.0
```
