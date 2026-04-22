#!/usr/bin/env bash
# End-to-end demo: two Rooms, namespace isolation, per-(Room,Agent) quotas,
# one shared HTTP connection pool. See DEMO_PLAN.md §验证清单.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

export HIVE_STATE="${HIVE_STATE:-/tmp/hive-demo}"
export HIVE_SOCKET="${HIVE_SOCKET:-$HIVE_STATE/hived.sock}"

cleanup() {
    set +e
    [ -n "${HIVED_PID:-}" ] && kill "$HIVED_PID" 2>/dev/null
    [ -n "${HTTP_PID:-}"  ] && kill "$HTTP_PID"  2>/dev/null
    wait 2>/dev/null
}
trap cleanup EXIT

say() { printf "\n\033[1;34m▶\033[0m %s\n" "$*"; }

say "1/11 build"
make build >/dev/null
rm -rf "$HIVE_STATE"
mkdir -p "$HIVE_STATE"

say "2/11 start local HTTP server on :8991 (so the fetch demo is offline-safe)"
python3 -m http.server 8991 --directory /tmp >/dev/null 2>&1 &
HTTP_PID=$!
sleep 0.3

say "3/11 start hived"
./bin/hived >"$HIVE_STATE/daemon.log" 2>&1 &
HIVED_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -S "$HIVE_SOCKET" ] && break
    sleep 0.2
done
./bin/hive ping

say "4/11 build four Agent images (3 binary + 1 skill)"
./bin/hive build ./examples/fetch     | sed 's/^/  /'
./bin/hive build ./examples/upper     | sed 's/^/  /'
./bin/hive build ./examples/summarize | sed 's/^/  /'
./bin/hive build ./examples/brief     | sed 's/^/  /'

say "5/11 up two Rooms with the same three Agents (namespace-isolated)"
ROOM_A=$(./bin/hive up hivefiles/demo/room-a.yaml)
ROOM_B=$(./bin/hive up hivefiles/demo/room-b.yaml)
echo "   ROOM_A=$ROOM_A"
echo "   ROOM_B=$ROOM_B"

say "6/11 Room A: consume fetch quota (intern has 5 http calls)"
for i in 1 2 3 4 5; do
    echo "   fetch #$i:"
    ./bin/hive run "$ROOM_A" --target fetch '{"url":"http://127.0.0.1:8991/"}' | \
        grep -E 'output:|STATUS|INFO' | head -3 | sed 's/^/     /'
done

say "   Room A: 6th fetch should be rejected by quota (observing enforcement)"
# Capture first; pipefail would otherwise propagate hive's non-zero exit and
# mask the grep result.
out=$(./bin/hive run "$ROOM_A" --target fetch '{"url":"http://127.0.0.1:8991/"}' 2>&1 || true)
if echo "$out" | grep -qi 'quota'; then
    echo "   ✓ quota rejected as expected: $(echo "$out" | grep -i quota | head -1)"
else
    echo "   ✗ unexpected: $out"
fi

say "7/11 Room B: one fetch succeeds (independent quota)"
./bin/hive run "$ROOM_B" --target fetch '{"url":"http://127.0.0.1:8991/"}' | \
    grep -E 'output:|INFO' | head -3 | sed 's/^/  /'

say "   summarize in both Rooms (deducts tokens independently)"
./bin/hive run "$ROOM_A" --target summarize '{"text":"the quick brown fox jumps over the lazy dog"}' | \
    grep -E 'output:|INFO' | head -5 | sed 's/^/  A: /'
./bin/hive run "$ROOM_B" --target summarize '{"text":"machine learning is a subset of artificial intelligence"}' | \
    grep -E 'output:|INFO' | head -5 | sed 's/^/  B: /'

say "8/11 kind=skill Agent: a SKILL.md Agent runs via the built-in runner"
ROOM_C=$(./bin/hive init skill-demo)
./bin/hive hire "$ROOM_C" brief:0.1.0 >/dev/null
echo "   Room $ROOM_C hired brief:0.1.0 (kind=skill, SKILL.md driven)"
./bin/hive run "$ROOM_C" '{"text":"Hive 是一套面向多 Agent AI 的能力级虚拟化系统，类比 Docker for Agents，让专家 Agent 可以独立打包、分发、配额管控。"}' | \
    grep -E 'output:|INFO' | head -5 | sed 's/^/  brief: /'
./bin/hive stop "$ROOM_C" >/dev/null

say "9/11 remote pull: hire a skill Agent from the GitHub-hosted registry"
# This scene fetches registry/agents/brief from the live public repo.
# Requires network access to raw.githubusercontent.com; on failure we
# warn and continue instead of aborting the demo.
ROOM_D=$(./bin/hive init remote-demo)
if ./bin/hive hire "$ROOM_D" 'github://xxx1766/Hive-/registry/agents/brief' 2>&1 | sed 's/^/  /'; then
    ./bin/hive run "$ROOM_D" '{"text":"远端 pull 的 skill Agent 通过 raw.githubusercontent.com 拉到本地 store，然后走跟本地 Agent 完全一样的沙箱。"}' | \
        grep -E 'output:|INFO' | head -4 | sed 's/^/  remote-brief: /'
else
    echo "  (skipped — remote fetch failed; is the registry pushed to github.com/xxx1766/Hive- yet?)"
fi
./bin/hive stop "$ROOM_D" >/dev/null 2>&1 || true

say "10/11 kind=workflow: static flow.json (fetch → llm_complete via variable refs)"
./bin/hive build ./examples/url-summary | sed 's/^/  /'
ROOM_E=$(./bin/hive init workflow-demo)
./bin/hive hire "$ROOM_E" url-summary:0.1.0 >/dev/null
./bin/hive run "$ROOM_E" "{\"url\":\"http://127.0.0.1:8991/\"}" | \
    grep -E 'INFO|output:' | head -5 | sed -E 's/^/  url-summary: /; s/(.{200}).*/\1…/'
./bin/hive stop "$ROOM_E" >/dev/null

say "11/11 team snapshots: observe per-Room quota divergence"
printf "  \033[1mRoom A:\033[0m\n"
./bin/hive team "$ROOM_A" | sed 's/^/    /'
printf "\n  \033[1mRoom B:\033[0m\n"
./bin/hive team "$ROOM_B" | sed 's/^/    /'

./bin/hive stop "$ROOM_A" >/dev/null
./bin/hive stop "$ROOM_B" >/dev/null

printf "\n\033[1;32m✓ demo complete\033[0m — daemon log: %s\n" "$HIVE_STATE/daemon.log"
