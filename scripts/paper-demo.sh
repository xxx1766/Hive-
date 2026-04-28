#!/usr/bin/env bash
# Paper-writing assistant demo: 4-Agent team (scout / outline / writer / reviewer)
# sharing a paper-corpus Volume across two parallel paper projects (Rooms).
#
# What this demo argues vs. native Claude Code / ChatGPT:
#   - paper-corpus Volume persists the author's writing voice across sessions/Rooms
#   - 4 Agents have distinct Ranks + per-(Room,Agent) quotas (writer at manager has 80k tokens; scout at staff has 20k)
#   - reviewer mounts the draft :ro — Rank-as-policy enforced by the sandbox, not by prompt-engineering
#   - two papers (ICML + NeurIPS) run in parallel with isolated drafts but a shared corpus
#
# Runs in ~60s. Set OPENAI_API_KEY for real prose; else mock LLM produces "mock-summary: ..." text.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

export HIVE_STATE="${HIVE_STATE:-/tmp/hive-paper-demo}"
export HIVE_SOCKET="${HIVE_SOCKET:-$HIVE_STATE/hived.sock}"

cleanup() {
    set +e
    [ -n "${HIVED_PID:-}" ] && kill "$HIVED_PID" 2>/dev/null
    [ -n "${ARXIV_PID:-}" ] && kill "$ARXIV_PID" 2>/dev/null
    wait 2>/dev/null
}
trap cleanup EXIT

say() { printf "\n\033[1;34m▶\033[0m %s\n" "$*"; }

say "1/12 build"
make build >/dev/null
rm -rf "$HIVE_STATE"
mkdir -p "$HIVE_STATE"

if [ -z "${OPENAI_API_KEY:-}" ]; then
    printf "\033[1;33m  warning:\033[0m OPENAI_API_KEY unset → llmproxy falls back to MockProvider.\n"
    printf "  The pipeline runs end-to-end but section text will be 'mock-summary: …' rather than real prose.\n"
    printf "  Skill agents (outline/writer/reviewer) emit a single mock answer without ReAct tool calls.\n"
    printf "  Set OPENAI_API_KEY to see actual paper drafts and the full multi-step ReAct loop.\n"
fi

say "2/12 start arxiv stub on :8992 (offline-safe)"
python3 -m http.server 8992 --directory examples/paper-assistant/sample-arxiv >/dev/null 2>&1 &
ARXIV_PID=$!
sleep 0.3

say "3/12 start hived"
./bin/hived >"$HIVE_STATE/daemon.log" 2>&1 &
HIVED_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -S "$HIVE_SOCKET" ] && break
    sleep 0.2
done
./bin/hive ping

say "4/12 build the 4 paper-assistant Agents"
./bin/hive build ./examples/paper-assistant/scout    | sed 's/^/  /'
./bin/hive build ./examples/paper-assistant/outline  | sed 's/^/  /'
./bin/hive build ./examples/paper-assistant/writer   | sed 's/^/  /'
./bin/hive build ./examples/paper-assistant/reviewer | sed 's/^/  /'

say "5/12 create Volumes (paper-corpus shared, drafts per-project)"
./bin/hive volume create paper-corpus        2>&1 | sed 's/^/  /'
./bin/hive volume create paper-icml-draft    2>&1 | sed 's/^/  /'
./bin/hive volume create paper-neurips-draft 2>&1 | sed 's/^/  /'

say "   seed paper-corpus with the author's writing methodology + style notes"
cp examples/paper-assistant/sample-corpus/*.md "$HIVE_STATE/volumes/paper-corpus/"
ls "$HIVE_STATE/volumes/paper-corpus/" | grep -v '^memory$' | sed 's/^/  - /'

say "6/12 hire ICML team (4 agents, paper-corpus + paper-icml-draft mounted)"
ROOM_ICML=$(./bin/hive hire -f hivefiles/paper-assistant/icml-paper.yaml)
echo "   ROOM_ICML=$ROOM_ICML"

say "7/12 paper-scout: stub arxiv → /shared/draft/related.md"
./bin/hive run "$ROOM_ICML" --target paper-scout \
    '{"topic":"efficient attention for long-context language models"}' 2>&1 \
    | grep -E 'INFO|output:|status' | head -8 | sed 's/^/  /'
echo "   ─────  /shared/draft/related.md  ─────"
head -20 "$HIVE_STATE/volumes/paper-icml-draft/related.md" 2>/dev/null | sed 's/^/  │ /' \
    || echo "  │ (related.md not produced — check daemon.log)"

say "8/12 paper-outline: read corpus + related.md → outline.md"
./bin/hive run "$ROOM_ICML" --target paper-outline \
    '{"hypothesis":"learned key selection beats fixed sparsity by adapting to head specialisation"}' 2>&1 \
    | grep -E 'INFO|output:|answer' | head -6 | sed 's/^/  /'
echo "   ─────  /shared/draft/outline.md  ─────"
head -20 "$HIVE_STATE/volumes/paper-icml-draft/outline.md" 2>/dev/null | sed 's/^/  │ /' \
    || echo "  │ (outline.md not produced — likely no API key, mock LLM did not ReAct fs_write)"

say "9/12 paper-writer: draft Methods (Rank=manager, 80k tokens budget)"
./bin/hive run "$ROOM_ICML" --target paper-writer \
    '{"section":"methods"}' 2>&1 \
    | grep -E 'INFO|output:|answer' | head -6 | sed 's/^/  /'
echo "   ─────  /shared/draft/methods.md  ─────"
head -20 "$HIVE_STATE/volumes/paper-icml-draft/methods.md" 2>/dev/null | sed 's/^/  │ /' \
    || echo "  │ (methods.md not produced — same as above; needs API key)"

say "10/12 paper-reviewer (mounted /shared/draft :ro): annotations on stdout"
./bin/hive run "$ROOM_ICML" --target paper-reviewer \
    '{"section":"methods"}' 2>&1 \
    | grep -E 'INFO|output:|answer' | head -10 | sed 's/^/  /'

say "11/12 second project: NeurIPS — same agents, separate draft, shared corpus"
ROOM_NEURIPS=$(./bin/hive hire -f hivefiles/paper-assistant/neurips-paper.yaml)
echo "   ROOM_NEURIPS=$ROOM_NEURIPS"
./bin/hive run "$ROOM_NEURIPS" --target paper-outline \
    '{"hypothesis":"adaptive sparsity at attention-head granularity"}' 2>&1 \
    | grep -E 'INFO|output:|answer' | head -4 | sed 's/^/  neurips: /'
echo "   ── verify isolation: drafts are different on-disk dirs"
echo "      icml-draft:    $(ls "$HIVE_STATE/volumes/paper-icml-draft/"    2>/dev/null | grep -v memory | tr '\n' ' ')"
echo "      neurips-draft: $(ls "$HIVE_STATE/volumes/paper-neurips-draft/" 2>/dev/null | grep -v memory | tr '\n' ' ')"
echo "   ── verify sharing: paper-corpus is the same dir both Rooms read"
echo "      paper-corpus:  $(ls "$HIVE_STATE/volumes/paper-corpus/"        2>/dev/null | grep -v memory | tr '\n' ' ')"

say "12/12 hive team — quota divergence between the two Rooms"
printf "  \033[1mICML Room:\033[0m\n"
./bin/hive team "$ROOM_ICML" | sed 's/^/    /'
printf "\n  \033[1mNeurIPS Room:\033[0m\n"
./bin/hive team "$ROOM_NEURIPS" | sed 's/^/    /'

./bin/hive stop "$ROOM_ICML"    >/dev/null
./bin/hive stop "$ROOM_NEURIPS" >/dev/null

printf "\n\033[1;32m✓ paper-demo complete\033[0m — daemon log: %s\n" "$HIVE_STATE/daemon.log"
