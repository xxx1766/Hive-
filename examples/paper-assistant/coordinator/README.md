# paper-coordinator — auto-hire-junior demo

Demonstrates Hive's third-pillar feature: a **manager-rank Agent spawning a subordinate at runtime** via `hire_junior`. The coordinator doesn't write anything itself — it hires `paper-writer` on the fly with a carved token budget, then delegates the section task via `peer_send`.

## What you'll see

| Surface | What appears |
|---|---|
| HTTP UI kanban | One conversation, planned → active → done in ~30–60s (coordinator + writer combined; `peer_call` blocks the coordinator until the writer replies) |
| Timeline view | `fs_read style-notes.md`, `hire_junior paper-writer:0.1.0 (staff)`, `peer_call → paper-writer` (round 1, going), peer reply `paper-writer → paper-coordinator` (round 2, returning) |
| Team tab | Subordinate tree: `└─ paper-writer (hired by paper-coordinator)` |
| `hive team` quota | coordinator's `tokens:openai/gpt-5.4-mini` drops by 30 000 (carved to writer) |
| `paper-osdi-draft/design.md` | Real OSDI design section (~600 words), already on disk by the time the conversation is `done` |

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
                                              │ react loop:
                                              │   1. fs_read style-notes.md
                                              │   2. hire_junior(paper-writer, staff, +30k tokens)
                                              │      → daemon carves quota; Member.Parent="paper-coordinator"
                                              │   3. peer_call(to=paper-writer, payload={section:design})
                                              │      ─── BLOCKS HERE ───
                                              │      │
                                              │      ▼ (round 1: outbound peer)
                                              │   paper-writer.runFromPeer (~20–30s):
                                              │     - fs_read corpus
                                              │     - llm_complete (real GMI call)
                                              │     - fs_write /shared/draft/design.md
                                              │     - peer_send back to coordinator
                                              │      │
                                              │      ▼ (round 2: returning reply, awaiter wakes)
                                              │   peer_call returns the reply payload as tool result
                                              │   4. LLM weaves writer's report into final answer
                                              │   5. task.Reply
                                              ▼
                              conversation = done (transcript has both rounds)
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

## How `peer_call` makes this clean

Earlier this demo was timing-dependent: the coordinator's task ended as soon as the LLM emitted `{"answer":"Delegated…"}`, marking the conversation `done`. If `paper-writer` then tried to `peer_send` its result back, the daemon rejected it (`PeerSendIntercept` saw the terminal status). The writer's output landed on disk in the volume but never reached the transcript.

`peer_call` (added alongside this demo) closes that gap. It registers an awaiter for `(to, conv_id)` BEFORE issuing the outbound `peer_send`, then blocks the coordinator's react turn on a channel that the runner's peer-router goroutine fills when the matching reply arrives. The coordinator's task doesn't `task.Reply` until the writer's payload is in hand — so the conversation stays `active` long enough for the round-2 reply to land in the transcript, and the LLM gets the writer's output as a tool result it can include in the final answer.

Caveat: the awaiter has a 60s default timeout (`timeout_seconds` in args, max 300s). A pathologically slow downstream still fails the call rather than blocking forever — at which point the coordinator can fall back to `peer_send` (one-way) and let the result reach the user via the volume.

## Troubleshooting

- **`hire_junior failed: rank intern is not allowed to hire`** — the demo Hivefile pins the coordinator at `manager` rank. If you `--rank` overrode it to `staff`, that strips `HireAllowed`. Re-hire as manager.
- **`hire_junior failed: parent quota tokens:… insufficient`** — coordinator's quota too small. Bump the Hivefile's `tokens` from 80000.
- **`paper-writer:0.1.0 not found`** — forgot `./bin/hive build ./examples/paper-assistant/writer`. The coordinator looks the image up at hire time.
- **design.md never appears** — check `daemon.log` for `peer reply failed` (expected) AND check `$HIVE_STATE/rooms/$ROOM/logs/paper-writer.stderr.log` for actual writer-side errors. Common: token cap on the carved 30k was too tight for a long-form draft — bump it in `SKILL.md` step 2.
- **Same room can't be re-run twice** — the second conversation will fail to `hire_junior` because `paper-writer` already exists in the Room (image-name uniqueness). Stop the room (`hive stop`) and re-hire fresh.
