# paper-coordinator — auto-hire-junior demo

Demonstrates Hive's third-pillar feature: a **manager-rank Agent spawning a subordinate at runtime** via `hire_junior`. The coordinator doesn't write anything itself — it hires `paper-writer` on the fly with a carved token budget, then delegates the section task via `peer_send`.

## What you'll see

| Surface | What appears |
|---|---|
| HTTP UI kanban | One conversation, planned → active → done in ~10s (the coordinator's own work) |
| Timeline view | Tool calls: `fs_read style-notes.md`, `hire_junior paper-writer:0.1.0 (staff)`, `peer_send → paper-writer` |
| Team tab | Subordinate tree: `└─ paper-writer (hired by paper-coordinator)` |
| `hive team` quota | coordinator's `tokens:openai/gpt-5.4-mini` drops by 30 000 (carved to writer) |
| `paper-osdi-draft/design.md` | Real OSDI design section (~600 words), ~30s after the coordinator finishes |

## Setup (one-time)

```bash
# Build daemon + agents
make build
./bin/hive build ./examples/paper-assistant/coordinator
./bin/hive build ./examples/paper-assistant/writer        # coordinator hires this

# Volumes
./bin/hive volume create paper-osdi-corpus
./bin/hive volume create paper-osdi-draft

# Seed the OSDI-flavoured style corpus into paper-osdi-corpus/
HIVE_STATE="${HIVE_STATE:-$HOME/.hive}"
cp examples/paper-assistant-osdi/sample-corpus/*.md "$HIVE_STATE/volumes/paper-osdi-corpus/"

# LLM gateway (or set OPENAI_API_KEY for OpenAI direct)
export OPENAI_API_KEY="$GMI_API_KEY"
export OPENAI_BASE_URL=https://api.gmi-serving.com/v1
```

## Run

```bash
# 1. Hire the coordinator (and only the coordinator — that's the point)
ROOM=$(./bin/hive hire -f hivefiles/paper-assistant/coordinator-demo.yaml)
echo "$ROOM"

# 2. Open the UI
xdg-open http://127.0.0.1:8910        # or just point your browser there

# 3. In the UI:
#    - Pick "$ROOM" in the room selector
#    - Click "+ New Conversation"
#    - target = paper-coordinator
#    - input  = {"section": "design"}
#    - Submit
```

You can also drive it from the CLI:

```bash
./bin/hive run "$ROOM" --target paper-coordinator '{"section":"design"}'
```

## What happens internally

```
User → conversation/start → coordinator.task ─┐
                                              │ react loop (5–10s):
                                              │   1. fs_read style-notes.md
                                              │   2. hire_junior(paper-writer, staff, +30k tokens)  ◀─ daemon carves quota
                                              │      → daemon spawns paper-writer, sets Member.Parent = "paper-coordinator"
                                              │   3. peer_send(to=paper-writer, payload={section:design})  ◀─ round 1 in conv
                                              │   4. answer "Delegated…"
                                              ▼
                              conversation = done
                                              │
                              paper-writer.runFromPeer (~20–30s, runs to completion):
                                              │   - fs_read corpus
                                              │   - llm_complete (real GMI call)
                                              │   - fs_write /shared/draft/design.md  ◀─ real output lands here
                                              │   - peer_send back to coordinator → REJECTED (conv terminal)
                                              ▼
                              writer's reply fails silently (logged)
```

## Verifying the four checks

```bash
# Check 1: timeline contains hire_junior + peer_send hops
curl -s http://127.0.0.1:8910/api/rooms/$ROOM/conversations | jq '.[] | {tag, status, round_count}'

# Check 2: subordinate tree
curl -s http://127.0.0.1:8910/api/rooms/$ROOM | \
  jq '.members[] | {image, parent}'   # paper-writer should have parent="paper-coordinator"

# Check 3: design.md appeared
sleep 30
ls "$HIVE_STATE/volumes/paper-osdi-draft/"
head -30 "$HIVE_STATE/volumes/paper-osdi-draft/design.md"

# Check 4: parent's quota was carved
./bin/hive team "$ROOM"
# coordinator's tokens:openai/gpt-5.4-mini should be 80000 - 30000 - (own usage) ≈ 47000
```

## v1 limitation: subordinate's reply is timing-dependent

The coordinator's task converges (LLM emits `{"answer":...}`) and `task.Reply`s after step 4. That marks the **conversation `done`** — `PeerSendIntercept` then rejects any further round-counted hops. Two cases:

- **Fast subordinate** (e.g. tool error in <1s): writer's `peer_send` reply lands **before** the coordinator finishes step 4 → reply arrives as round 2 in the transcript. You'll see it in the timeline.
- **Slow subordinate** (normal 20–30s LLM draft): writer replies **after** the coordinator is done → reply gets rejected, daemon logs `peer reply failed: conversation … is done`. The writer's actual output is still on disk in the volume.

Both cases produce a real `design.md` in the volume — that's the demo's load-bearing artifact. To deterministically get the reply back into the transcript, the runner would need a synchronous `peer_call` tool that blocks the coordinator's react loop until the writer replies. That's a v2 feature; see `ARCHITECTURE.md` § "Auto-hire 与配额 carve".

## Troubleshooting

- **`hire_junior failed: rank intern is not allowed to hire`** — the demo Hivefile pins the coordinator at `manager` rank. If you `--rank` overrode it to `staff`, that strips `HireAllowed`. Re-hire as manager.
- **`hire_junior failed: parent quota tokens:… insufficient`** — coordinator's quota too small. Bump the Hivefile's `tokens` from 80000.
- **`paper-writer:0.1.0 not found`** — forgot `./bin/hive build ./examples/paper-assistant/writer`. The coordinator looks the image up at hire time.
- **design.md never appears** — check `daemon.log` for `peer reply failed` (expected) AND check `$HIVE_STATE/rooms/$ROOM/logs/paper-writer.stderr.log` for actual writer-side errors. Common: token cap on the carved 30k was too tight for a long-form draft — bump it in `SKILL.md` step 2.
- **Same room can't be re-run twice** — the second conversation will fail to `hire_junior` because `paper-writer` already exists in the Room (image-name uniqueness). Stop the room (`hive stop`) and re-hire fresh.
