#!/usr/bin/env bash
# Hive installer — builds the four binaries from source and installs them
# to $PREFIX/bin (default /usr/local/bin). Idempotent: rerun to upgrade
# an existing install. State dir (~/.hive/) is created with empty
# images/ and rooms/ subdirs; existing contents are preserved.
#
# Usage:
#   ./scripts/install.sh                    # sudo if PREFIX=/usr/local
#   PREFIX=$HOME/.local ./scripts/install.sh  # user-local, no sudo
#   ./scripts/install.sh --skip-build       # reuse existing bin/

set -euo pipefail

PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKIP_BUILD=0

for arg in "$@"; do
    case "$arg" in
        --skip-build) SKIP_BUILD=1 ;;
        --help|-h)
            sed -n '2,12p' "$0" | sed 's/^# //; s/^#$//'
            exit 0 ;;
        *) echo "unknown arg: $arg" >&2; exit 2 ;;
    esac
done

BINARIES=(hive hived hive-skill-runner hive-workflow-runner)

say() { printf "\033[1;34m▶\033[0m %s\n" "$*"; }
die() { echo "error: $*" >&2; exit 1; }

# ── Preflight ─────────────────────────────────────────────────────────────

[[ "$(uname -s)" == "Linux" ]] || die "Hive requires Linux"

if (( ! SKIP_BUILD )); then
    command -v go >/dev/null || die "'go' not found — install Go 1.22+ first"
    go_version=$(go version | awk '{print $3}' | sed 's/go//')
    major=$(echo "$go_version" | cut -d. -f1)
    minor=$(echo "$go_version" | cut -d. -f2)
    if (( major < 1 )) || (( major == 1 && minor < 22 )); then
        die "Go $go_version found; need >= 1.22"
    fi
fi

# ── Build ─────────────────────────────────────────────────────────────────

if (( ! SKIP_BUILD )); then
    say "building from $REPO_ROOT"
    (cd "$REPO_ROOT" && make build >/dev/null)
fi

# ── Install ───────────────────────────────────────────────────────────────

if [[ ! -d "$BIN_DIR" ]]; then
    mkdir -p "$BIN_DIR" 2>/dev/null || die "cannot create $BIN_DIR — rerun with sudo or set PREFIX"
fi
if [[ ! -w "$BIN_DIR" ]]; then
    die "$BIN_DIR is not writable as $(whoami) — rerun with sudo or set PREFIX=\$HOME/.local"
fi

say "installing to $BIN_DIR"
for b in "${BINARIES[@]}"; do
    [[ -f "$REPO_ROOT/bin/$b" ]] || die "missing $REPO_ROOT/bin/$b — did 'make build' succeed?"
    install -m 755 "$REPO_ROOT/bin/$b" "$BIN_DIR/$b"
    echo "  $BIN_DIR/$b"
done

# ── State dir (idempotent) ────────────────────────────────────────────────

STATE_DIR="${HIVE_STATE:-$HOME/.hive}"
mkdir -p "$STATE_DIR/images" "$STATE_DIR/rooms"

# Breadcrumb for `hive update`: lets the CLI find this source tree later
# without asking the user. Re-run install.sh from a different checkout to
# point future updates at the new tree.
INSTALLED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
cat > "$STATE_DIR/install.json" <<EOF
{"source_dir":"$REPO_ROOT","prefix":"$PREFIX","installed_at":"$INSTALLED_AT"}
EOF

# ── Next steps ────────────────────────────────────────────────────────────

printf "\n\033[1;32m✓ installed\033[0m — binaries in %s, state in %s\n\n" "$BIN_DIR" "$STATE_DIR"
cat <<EOF
Next steps:
  1. start the daemon (needs root for namespaces):
       sudo hived &

  2. verify:
       hive version

  3. hire a skill Agent from the public registry and run it:
       ROOM=\$(hive init demo)
       hive pull github://xxx1766/Hive-/registry/agents/brief
       hive hire "\$ROOM" brief:0.1.0
       hive run  "\$ROOM" '{"text":"Hive 是一套面向多 Agent AI 的能力级虚拟化系统。"}'

Full walkthrough: docs/TUTORIAL.md
EOF
