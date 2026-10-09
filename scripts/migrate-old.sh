#!/bin/bash
# Migrate legacy whole-file .enc (old aes.go format):
#   ./scripts/migrate-old.sh movie.enc [name]
# Output goes to $VIDEOS_DIR (default: videos/).
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -lt 1 ]]; then echo "usage: $0 OLD.enc [name]"; exit 2; fi
IN="$1"
NAME="${2:-$(basename "$IN" | sed 's/\.[^.]*$//')}"
NAME="$(echo "$NAME" | tr ' ' '_' | tr -cd 'A-Za-z0-9._-')"
VIDEOS_DIR="${VIDEOS_DIR:-videos}"
mkdir -p "$VIDEOS_DIR"
OUT="$VIDEOS_DIR/${NAME}.cenc"

[[ -f "$IN" ]] || { echo "input not found: $IN"; exit 1; }
[[ -f keys.json ]] || { echo "no keys.json — run ./scripts/keygen.sh first (must be the SAME key used to encrypt)"; exit 1; }
[[ -e "$OUT" ]] && { echo "$OUT exists. rm it first."; exit 1; }
[[ -x bin/encrypt ]] || ./scripts/build.sh

./bin/encrypt --migrate-old --in "$IN" --out "$OUT"
echo "done. Open http://127.0.0.1:8080 and pick '$NAME'."
