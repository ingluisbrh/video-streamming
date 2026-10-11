#!/bin/bash
# Decrypt a .cenc back to the original file:
#   ./scripts/decrypt.sh NAME [outdir]     (reads $VIDEOS_DIR/NAME.cenc, default videos/)
#   ./scripts/decrypt.sh path/to/file.cenc [outdir]
# Needs the SAME key (keys.json) that was used to encrypt.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -lt 1 ]]; then echo "usage: $0 NAME-or-file.cenc [outdir]"; exit 2; fi
SRC="$1"
VIDEOS_DIR="${VIDEOS_DIR:-videos}"
if [[ -f "$SRC" ]]; then
  IN="$SRC"
else
  NAME="$(echo "$SRC" | tr ' ' '_' | tr -cd 'A-Za-z0-9._-')"
  IN="$VIDEOS_DIR/${NAME%.cenc}.cenc"
fi
OUTDIR="${2:-.}"
NAME="$(basename "$IN" | sed 's/\.[^.]*$//')"
OUT="$OUTDIR/${NAME}"

[[ -f "$IN" ]] || { echo "input not found: $IN"; exit 1; }
[[ -f keys.json ]] || { echo "no keys.json — run ./scripts/keygen.sh first (must be the SAME key used to encrypt)"; exit 1; }
mkdir -p "$OUTDIR"
[[ -e "$OUT" ]] && { echo "$OUT exists. rm it first or pick another outdir."; exit 1; }
[[ -x bin/encrypt ]] || ./scripts/build.sh

./bin/encrypt --decrypt --in "$IN" --out "$OUT"
echo "done. Decrypted copy: $OUT"
