# paper-supervisor — `peer_call_many` fan-out + alias demo

Showcases two Hive features together:

1. **Parallel fan-out** — a manager-rank coordinator calls N peers concurrently via `peer_call_many`. Awaiter registry holds N entries keyed by `(from, conv_id)`, each goroutine dispatches its own reply.
2. **Member-name aliasing** — both peers run the **same image** (`paper-reviewer:0.1.0`) but get distinct in-room aliases (`reviewer-anti-pattern`, `reviewer-style`). Daemon dedups by `Member.Name`, not `Image.Name`, so `hire_junior --name <alias>` lets one image be hired N times.

## What you'll see

| Surface | What appears |
|---|---|
| HTTP UI kanban | One conversation, planned → active → done in ~max(reviewer time), not sum. With two ~30s reviewers, ~30s total (vs 60s if you'd called them sequentially). |
| Timeline view | `hire_junior paper-reviewer:0.1.0 name=reviewer-anti-pattern`, `hire_junior paper-reviewer:0.1.0 name=reviewer-style`, `peer_call_many` → two concurrent outbound peers (rounds 1 & 2), then two inbound replies (rounds 3 & 4). |
| Team tab | Subordinate tree: `paper-supervisor` with two children `└─ reviewer-anti-pattern (paper-reviewer)` and `└─ reviewer-style (paper-reviewer)` — both running the same image but distinguished by alias. |
| Final answer | Aggregated report quoting both reviewers; the LLM uses the focus hint each was given to nudge them into complementary angles. |

## Setup

```bash
make build
./bin/hive build ./examples/paper-assistant/supervisor
./bin/hive build ./examples/paper-assistant/reviewer       # both aliases run this image

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
m1  round 0  task_input    creator → paper-supervisor                  {"section":"design"}
m2  round 1  peer          paper-supervisor → reviewer-anti-pattern    {"section":"design", "focus":"anti-patterns ..."}
m3  round 2  peer          paper-supervisor → reviewer-style           {"section":"design", "focus":"voice ..."}
m4  round 3  peer          reviewer-anti-pattern → paper-supervisor    {answer: "...findings..."}
m5  round 4  peer          reviewer-style → paper-supervisor           {answer: "...findings..."}
m6  round —  task_output   paper-supervisor → -                        {answer: aggregated report}
```

`from`/`to` show the **alias**, not the image — both reviewers are running `paper-reviewer:0.1.0` but addressable by their distinct names. m2/m3 may be in either order (concurrent goroutines); same for m4/m5 (whoever's LLM finished first arrives first).

## Troubleshooting

- **`hire_junior failed: parent quota tokens:openai/gpt-5.4-mini insufficient`** — supervisor's quota is 30k; two carves of 8k + own usage might push it. Bump to 50k in the Hivefile.
- **`peer_call_many failed: timeout after 180s`** — one reviewer is stuck or quota-exhausted. Check `$HIVE_STATE/rooms/$ROOM/logs/paper-reviewer.stderr.log` and `paper-style-critic.stderr.log`.
- **`fs_read /shared/draft/design.md: no such file`** — you skipped the prerequisite (run the coordinator demo first to create design.md).
- **One reviewer reports `ok: false` in the result** — supervisor's SKILL.md instructs it to surface this in the final answer. Look at the `error` field.

## See also

- `examples/paper-assistant/coordinator/` — single-junior pattern (sequential `peer_call`)
- `ARCHITECTURE.md` § "Auto-hire 与配额 carve" — rank policy + quota carve invariants
- `cmd/hive-skill-runner/main.go` — `dispatchPeerCallMany` implementation
