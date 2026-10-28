#!/usr/bin/env bash
set -euo pipefail

SRC="${1:-./px0}"

if [ ! -f "$SRC" ]; then
    echo "Error: Source binary '$SRC' not found. Run 'go build' first." >&2
    exit 1
fi

SRC_REAL=$(readlink -f "$SRC" 2>/dev/null || realpath "$SRC" 2>/dev/null || echo "$SRC")
SEEN=""
REPLACED=0

TARGETS=()

if [ -n "${INSTALL_PATH:-}" ]; then
    TARGETS+=("$INSTALL_PATH")
fi

# Scan PATH for existing px0 binaries
SAVED_IFS="$IFS"
IFS=:
for dir in $PATH; do
    [ -z "$dir" ] && continue
    candidate="$dir/px0"
    if [ -f "$candidate" ] || [ -L "$candidate" ]; then
        TARGETS+=("$candidate")
    fi
done
IFS="$SAVED_IFS"

# Also check ~/.local/bin/px0 as a fallback if it exists
if [ -f "$HOME/.local/bin/px0" ] || [ -L "$HOME/.local/bin/px0" ]; then
    TARGETS+=("$HOME/.local/bin/px0")
fi

for candidate in "${TARGETS[@]}"; do
    destdir=$(dirname "$candidate")
    mkdir -p "$destdir" 2>/dev/null || true

    # Resolve canonical destination if it exists
    real=$(readlink -f "$candidate" 2>/dev/null || realpath "$candidate" 2>/dev/null || echo "$candidate")
    [ -z "$real" ] && continue

    # Do not replace the source binary itself in the repository
    if [ "$real" = "$SRC_REAL" ]; then
        continue
    fi

    # Deduplicate in case multiple PATH entries resolve to the same binary
    case " $SEEN " in
        *" $real "*) continue ;;
        *) SEEN="$SEEN $real" ;;
    esac

    destdir=$(dirname "$real")
    tmp="$destdir/.px0-update.$$"

    # Atomic replacement via temporary file in the same directory (safe for running processes)
    if cp -f "$SRC" "$tmp" 2>/dev/null && chmod 755 "$tmp" 2>/dev/null && mv -f "$tmp" "$real" 2>/dev/null; then
        echo "✓ Replaced existing binary at $real"
        REPLACED=$((REPLACED + 1))
    elif cp -f "$SRC" "$real" 2>/dev/null && chmod 755 "$real" 2>/dev/null; then
        echo "✓ Replaced existing binary at $real"
        REPLACED=$((REPLACED + 1))
    else
        echo "! Notice: Cannot write to $real (permission denied - run with sudo if needed)" >&2
    fi
    rm -f "$tmp" 2>/dev/null
done

if [ "$REPLACED" -eq 0 ] && [ -z "${INSTALL_PATH:-}" ]; then
    echo "ℹ No existing px0 binary found in PATH or ~/.local/bin to replace."
fi
