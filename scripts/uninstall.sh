#!/usr/bin/env bash
# Hive uninstaller — removes the four binaries from $PREFIX/bin.
# With --purge also wipes the ~/.hive state directory.
#
# Usage:
#   ./scripts/uninstall.sh            # remove binaries only
#   ./scripts/uninstall.sh --purge    # also delete ~/.hive
#   PREFIX=$HOME/.local ./scripts/uninstall.sh

set -euo pipefail

PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
PURGE=0

for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=1 ;;
        --help|-h)
            sed -n '2,9p' "$0" | sed 's/^# //; s/^#$//'
            exit 0 ;;
        *) echo "unknown arg: $arg" >&2; exit 2 ;;
    esac
done

BINARIES=(hive hived hive-skill-runner hive-workflow-runner)

for b in "${BINARIES[@]}"; do
    if [[ -e "$BIN_DIR/$b" ]]; then
        rm -f "$BIN_DIR/$b"
        echo "  removed $BIN_DIR/$b"
    fi
done

if (( PURGE )); then
    STATE_DIR="${HIVE_STATE:-$HOME/.hive}"
    if [[ -d "$STATE_DIR" ]]; then
        rm -rf "$STATE_DIR"
        echo "  removed $STATE_DIR"
    fi
fi

echo "✓ uninstalled"
