#!/bin/bash
# Add an image: ./scripts/add-image.sh INPUT.jpg [name]
# Encrypts to $IMAGES_DIR/<name>.cenc (default: images/).
# The original extension is kept in the name so the server can fall back to it
# when sniffing the Content-Type.
# IMAGES_DIR=/mnt/secure/pics ./scripts/add-image.sh photo.jpg
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -lt 1 ]]; then echo "usage: $0 INPUT.jpg [name]"; exit 2; fi
IN="$1"
# Default name = full basename (extension included); it is not stripped.
NAME="${2:-$(basename "$IN")}"
# sanitize: keep alnum dot dash underscore
NAME="$(echo "$NAME" | tr ' ' '_' | tr -cd 'A-Za-z0-9._-')"
IMAGES_DIR="${IMAGES_DIR:-images}"
mkdir -p "$IMAGES_DIR"
OUT="$IMAGES_DIR/${NAME}.cenc"

[[ -f "$IN" ]] || { echo "input not found: $IN"; exit 1; }
[[ -f keys.json ]] || { echo "no keys.json — run ./scripts/keygen.sh first"; exit 1; }
[[ -e "$OUT" ]] && { echo "$OUT exists. rm it first or pick another name."; exit 1; }
[[ -x bin/encrypt ]] || ./scripts/build.sh

./bin/encrypt --in "$IN" --out "$OUT"
echo "done. Open http://127.0.0.1:8080 and view '$NAME'."
