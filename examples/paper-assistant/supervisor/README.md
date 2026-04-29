# paper-supervisor — `peer_call_many` fan-out demo

Showcases Hive's parallel-fan-out pattern: a manager-rank coordinator hires **two distinct specialist reviewers** at runtime, calls **both concurrently** via `peer_call_many`, and aggregates their feedback into one report.

The architectural claim: the awaiter registry in skill-runner is keyed by `(from, conv_id)` — two awaiters with different `from` peers cohabit the registry and are dispatched independently when their replies arrive. `peer_call_many` exercises this directly.

## What you'll see

| Surface | What appears |
|---|---|
| HTTP UI kanban | One conversation, planned → active → done in ~max(reviewer time), not sum. With two ~30s reviewers, ~30s total (vs 60s if you'd called them sequentially). |
| Timeline view | `hire_junior paper-reviewer`, `hire_junior paper-style-critic`, `peer_call_many → [paper-reviewer, paper-style-critic]` (two outbound peer hops, rounds 1 & 2), then two inbound reply peers (rounds 3 & 4). |
| Team tab | Subordinate tree: `paper-supervisor` with two children `└─ paper-reviewer (hired by paper-supervisor)` and `└─ paper-style-critic (hired by paper-supervisor)`. |
| Final answer | Aggregated report mentioning both `structural_issues` (from paper-reviewer) and `style_issues` (from paper-style-critic). |

## Setup

```bash
make build
./bin/hive build ./examples/paper-assistant/supervisor
./bin/hive build ./examples/paper-assistant/style-critic
./bin/hive build ./examples/paper-assistant/reviewer

./bin/hive volume create paper-osdi-corpus
./bin/hive volume create paper-osdi-draft

HIVE_STATE="${HIVE_STATE:-$HOME/.hive}"
cp examples/paper-assistant-osdi/sample-corpus/*.md "$HIVE_STATE/volumes/paper-osdi-corpus/"
```

You also need `paper-osdi-draft/design.md` to exist before the supervisor can review it. Easiest path: run the coordinator demo first.

```bash
ROOM_COORD=$(./bin/hive hire -f hivefiles/paper-assistant/coordinator-demo.yaml)
# Drive a conversation through paper-coordinator with {"section":"design"}, wait ~60s.
# When `design.md` shows up in $HIVE_STATE/volumes/paper-osdi-draft/, stop the room.
./bin/hive stop "$ROOM_COORD"
```

## Run

```bash
ROOM=$(./bin/hive hire -f hivefiles/paper-assistant/supervisor-demo.yaml)

# UI: http://127.0.0.1:8910 → "+ New Conversation"
#     target = paper-supervisor
#     input  = {"section": "design"}
```

Or via CLI:
```bash
./bin/hive run "$ROOM" --target paper-supervisor '{"section":"design"}'
```

## Why parallel matters here

`peer_call_many` registers all N awaiters BEFORE any send and spawns N goroutines that PeerSend + await concurrently. Total wall-time is `max(reply_i)` across all calls plus a tiny IPC roundtrip. With sequential `peer_call` you'd get `sum(reply_i)`.

For two ~30s reviewers, that's 30s vs 60s — meaningful when the supervisor pattern scales to N=4 or N=8 reviewers.

## Expected transcript

After a clean run, the conversation has 6 messages:

```
m1  round 0  task_input    creator → paper-supervisor               {"section":"design"}
m2  round 1  peer          paper-supervisor → paper-reviewer        {"section":"design"}
m3  round 2  peer          paper-supervisor → paper-style-critic    {"section":"design"}
m4  round 3  peer          paper-reviewer → paper-supervisor        {answer: {...checklist findings...}}
m5  round 4  peer          paper-style-critic → paper-supervisor    {answer: {style_issues, tone, score}}
m6  round —  task_output   paper-supervisor → -                     {answer: aggregated report}
```

m2 and m3 may be in either order (both are sent in concurrent goroutines). m4 and m5 likewise — order reflects which reviewer's LLM finished first. The transcript ordering is by daemon receipt time, not by call order — that's correct for "what happened in the room" semantics.

## Troubleshooting

- **`hire_junior failed: parent quota tokens:openai/gpt-5.4-mini insufficient`** — supervisor's quota is 30k; two carves of 8k + own usage might push it. Bump to 50k in the Hivefile.
- **`peer_call_many failed: timeout after 180s`** — one reviewer is stuck or quota-exhausted. Check `$HIVE_STATE/rooms/$ROOM/logs/paper-reviewer.stderr.log` and `paper-style-critic.stderr.log`.
- **`fs_read /shared/draft/design.md: no such file`** — you skipped the prerequisite (run the coordinator demo first to create design.md).
- **One reviewer reports `ok: false` in the result** — supervisor's SKILL.md instructs it to surface this in the final answer. Look at the `error` field.

## See also

- `examples/paper-assistant/coordinator/` — single-junior pattern (sequential `peer_call`)
- `ARCHITECTURE.md` § "Auto-hire 与配额 carve" — rank policy + quota carve invariants
- `cmd/hive-skill-runner/main.go` — `dispatchPeerCallMany` implementation
