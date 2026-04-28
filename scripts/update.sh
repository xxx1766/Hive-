#!/usr/bin/env bash
# Hive updater — pulls the latest source, rebuilds the four binaries, and
# reinstalls them via scripts/install.sh. Equivalent to `hive update` but
# usable when the hive binary is missing or broken (bootstrap path).
#
# Usage:
#   ./scripts/update.sh                     # pull + build + install
#   ./scripts/update.sh --check             # compare local vs upstream only
#   ./scripts/update.sh --force             # rebuild even if already at HEAD
#   ./scripts/update.sh --ref <branch|tag|sha>
#   PREFIX=$HOME/.local ./scripts/update.sh
#
# Operates on the source tree this script lives in (REPO_ROOT). To update
# from a different checkout, run that checkout's scripts/update.sh.

set -euo pipefail

PREFIX="${PREFIX:-/usr/local}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK=0
FORCE=0
REF=""

while (( $# )); do
    case "$1" in
        --check) CHECK=1 ;;
        --force) FORCE=1 ;;
        --ref) REF="${2:-}"; shift ;;
        --ref=*) REF="${1#--ref=}" ;;
        --prefix) PREFIX="${2:-}"; shift ;;
        --prefix=*) PREFIX="${1#--prefix=}" ;;
        --help|-h)
            sed -n '2,15p' "$0" | sed 's/^# //; s/^#$//'
            exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
    shift
done

say() { printf "\033[1;34m▶\033[0m %s\n" "$*"; }
die() { echo "error: $*" >&2; exit 1; }

# ── Preflight ─────────────────────────────────────────────────────────────

[[ "$(uname -s)" == "Linux" ]] || die "Hive requires Linux"
[[ -d "$REPO_ROOT/.git" ]] || die "$REPO_ROOT is not a git working tree"
[[ -f "$REPO_ROOT/Makefile" ]] || die "$REPO_ROOT has no Makefile"
[[ -x "$REPO_ROOT/scripts/install.sh" ]] || die "$REPO_ROOT/scripts/install.sh missing"
command -v git >/dev/null || die "'git' not found"

cd "$REPO_ROOT"

# ── Optional: switch to a specific ref before fetching ───────────────────

if [[ -n "$REF" ]]; then
    say "checking out $REF"
    git checkout "$REF"
fi

# ── Fetch + compare ───────────────────────────────────────────────────────

say "fetching origin"
git fetch --quiet

LOCAL=$(git rev-parse --short HEAD)
UPSTREAM=$(git rev-parse --short '@{u}' 2>/dev/null) || \
    die "current branch has no upstream — set one with 'git branch --set-upstream-to=...' or pass --ref"

printf "local:    %s\n" "$LOCAL"
printf "upstream: %s\n" "$UPSTREAM"

if (( CHECK )); then
    if [[ "$LOCAL" == "$UPSTREAM" ]]; then
        echo "up to date."
    else
        echo "update available — run 'scripts/update.sh' (or 'hive update') to apply."
    fi
    exit 0
fi

if [[ "$LOCAL" == "$UPSTREAM" && ! $FORCE -eq 1 ]]; then
    echo "already up to date."
    exit 0
fi

# ── Pull (ff-only protects in-progress local work) ───────────────────────

if [[ "$LOCAL" != "$UPSTREAM" ]]; then
    say "git pull --ff-only"
    git pull --ff-only || die "git pull failed — resolve any local changes manually, then rerun"
fi

# ── Build + install (delegate to install.sh's --skip-build path) ─────────

say "building"
make build

say "installing (PREFIX=$PREFIX)"
PREFIX="$PREFIX" "$REPO_ROOT/scripts/install.sh" --skip-build

NEW=$(git rev-parse --short HEAD)
printf "\n\033[1;32m✓ updated\033[0m %s → %s\n" "$LOCAL" "$NEW"

# ── Daemon restart hint ───────────────────────────────────────────────────
# Probe the same socket paths internal/ipc/paths.go uses. The running
# daemon keeps its mmap'd binary, so the new hived only takes effect on
# next start.

SOCK="${HIVE_SOCKET:-}"
if [[ -z "$SOCK" ]]; then
    if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
        SOCK="$XDG_RUNTIME_DIR/hive/hived.sock"
    else
        SOCK="$HOME/.hive/hived.sock"
    fi
fi
if [[ -S "$SOCK" ]]; then
    cat <<EOF

note: hived is still running the previous version. To pick up the update:
        sudo killall hived && sudo hived &
EOF
fi
